//go:build !ios_extension && !js

package sdk

// Claiming a Top 200 spot from one tap, on the device that holds the network's
// keys (apple BTEASY.md, "The flow, from one tap"). Every platform runs this code.
// Apps own only the secret store, the wallet transports and the screens.
//
// Tap starts or resumes the whole flow. Advance does what needs no tap: publishing
// a commitment and signing bindings, both free, which is all renewing and adding
// devices take. Advance also finishes a first claim the user already tapped, since
// that claim can wait days for its window to register in, and one tap means one tap.
// A UID lost after that takes a new tap, as it spends again.
//
// Nothing here keeps state of its own between calls. It reads the server and the
// chain and does the next thing, so a crash, a lost phone connection or a second
// call all resume in the same place.
//
//	Step                                  Who signs           Needs a tap
//	create the hotkey                     nobody              yes
//	authorize the hotkey                  the coldkey         yes
//	convert alpha, or ask for the top-up  the coldkey         the first one
//	register the hotkey                   the coldkey         the first one
//	publish the manifest's commitment     the hotkey          no
//	sign each device's binding            the hotkey          no
//
// The server chooses the devices and builds the manifest, and the coordinator's
// oracle mirrors the commitment. The server hands this code a manifest and
// bindings, and this code never signs what it was handed: it reads them, recomputes
// the digest and commitment itself, and signs only one fixed shape, so a compromised
// server can't get anything else signed. It can only ask for a binding of a device
// in a manifest it names. Every chain write builds one fixed call from typed values.
//
// A network whose coldkey is in a wallet stops at two points, a message and a
// transaction, each once (question 17). After that the flow is the same.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk/sn/evm"
	"github.com/urnetwork/sdk/sn/protocol"
	"github.com/urnetwork/sdk/sn/ss58"
)

// Where a claim stands, in SnFleetClaimState.Stage.
const (
	// binding isn't open on mainnet yet
	SnFleetClaimStageClosed = "closed"
	// the network has no payout consent: set up payouts first
	SnFleetClaimStageNoConsent = "no_consent"
	// nothing started, and starting takes a tap
	SnFleetClaimStageNeedsTap = "needs_tap"
	// the fleet's hotkey is on another device of this network, so this one can't
	// renew or add devices
	SnFleetClaimStageHotkeyElsewhere = "hotkey_elsewhere"
	// the wallet must sign WalletMessage; then SubmitWalletAuthorization
	SnFleetClaimStageNeedsWalletAuthorization = "needs_wallet_authorization"
	// registering now would leave the new UID exposed before its binding starts;
	// the flow resumes at RegisterAfterBlock
	SnFleetClaimStageWaitingRegisterWindow = "waiting_register_window"
	// the live burn spiked; it halves about every 72 minutes
	SnFleetClaimStageWaitingBurn = "waiting_burn"
	// the top-up was sent and hasn't landed yet
	SnFleetClaimStageWaitingTopUp = "waiting_top_up"
	// alpha is being converted to pay the burn
	SnFleetClaimStageConverting = "converting"
	// too little to register, too little alpha to convert, and no top-up to ask for
	SnFleetClaimStageNeedsFunds = "needs_funds"
	// the wallet must register HotkeyAddress on Netuid, within BurnLimitRao
	SnFleetClaimStageNeedsWalletRegistration = "needs_wallet_registration"
	// the registration was sent and isn't on chain yet
	SnFleetClaimStageRegistering = "registering"
	// the server is choosing the devices
	SnFleetClaimStageChoosingDevices = "choosing_devices"
	// the commitment is published, and the oracle hasn't mirrored it yet
	SnFleetClaimStageWaitingMirror = "waiting_mirror"
	// chosen devices haven't signed yet
	SnFleetClaimStageWaitingDevices = "waiting_devices"
	// every chosen device is bound
	SnFleetClaimStageBound = "bound"
	// see ErrorCode
	SnFleetClaimStageFailed = "failed"
)

// Stable codes in SnFleetClaimState.ErrorCode, for localization.
const (
	// the app's secret store failed, so nothing was signed
	SnFleetClaimErrorSecretStore = "secret_store_failed"
	// a server call failed; try again
	SnFleetClaimErrorServer = "server_unavailable"
	// a chain read or write failed; try again
	SnFleetClaimErrorChain = "chain_unavailable"
	// the server refused the authorization
	SnFleetClaimErrorAuthorizationRefused = "authorization_refused"
	// the wallet's signature isn't over this network's authorization by its coldkey
	SnFleetClaimErrorWalletSignature = "wallet_signature_invalid"
	// what the server sent can't be signed, so nothing was
	SnFleetClaimErrorBadManifest = "bad_manifest"
	SnFleetClaimErrorBadBinding  = "bad_binding"
	// the server refused the top-up
	SnFleetClaimErrorTopUpRefused = "top_up_refused"
)

const (
	// A burn above this waits. The floor is 0.0005 TAO, each registration multiplies
	// it by 1.26, and it halves every 360 blocks, so a higher one means a wave of
	// registrations that passes within hours. The server caps a top-up at the same
	// amount.
	SnFleetClaimMaxBurnRao int64 = protocol.FleetClaimMaxBurnRao

	// a transaction can wait its whole era to land, and then for finality
	snFleetClaimStepTimeout = 20 * time.Minute

	snFleetClaimRetryShort = 2 * 60
	snFleetClaimRetryLong  = 30 * 60
)

// SnFleetClaimStore is the app's secret store. It holds what must never reach the
// server, a log, a file or the tunnel extension, keyed by network id, and survives
// logout the way the recovery phrase does.
type SnFleetClaimStore interface {
	// the network's fleet hotkey seed as hex, "" when there's none. On a read
	// failure, any other text: it fails to parse and stops the flow, where a ""
	// would make a new hotkey.
	LoadSnFleetHotkeySeed(networkId string) string
	// true once written and read back
	SaveSnFleetHotkeySeed(networkId string, seedHex string) bool
	// the recovery phrase of the network's key on this device, "" when a wallet
	// holds the coldkey or the key was deleted
	LoadSnFleetColdkeyPhrase(networkId string) string
}

// Where a claim stands, for the card.
type SnFleetClaimState struct {
	Stage        string `json:"stage"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	// ask again after this long when not 0
	RetryAfterSeconds int64 `json:"retry_after_seconds,omitempty"`
	// the board prompts this network to claim
	ClaimPrompt    bool   `json:"claim_prompt"`
	ColdkeyAddress string `json:"coldkey_address,omitempty"`
	HotkeyAddress  string `json:"hotkey_address,omitempty"`
	Netuid         int64  `json:"netuid,omitempty"`
	Registered     bool   `json:"registered"`
	Uid            int64  `json:"uid,omitempty"`
	// with SnFleetClaimStageNeedsWalletAuthorization, the exact text to sign
	WalletMessage string `json:"wallet_message,omitempty"`
	// with SnFleetClaimStageNeedsWalletRegistration, the most the burn may be
	BurnLimitRao       int64 `json:"burn_limit_rao,omitempty"`
	RegisterAfterBlock int64 `json:"register_after_block,omitempty"`
	CurrentEpoch       int64 `json:"current_epoch,omitempty"`
	// the server chose these, one per IP prefix, up to the per-fleet share
	Chosen int64 `json:"chosen"`
	// chosen devices that signed, or are bound already
	Ready int64 `json:"ready"`
	// chosen devices bound on chain, including those starting next epoch
	Bound int64 `json:"bound"`
	// chosen devices that refused; the next batch replaces them
	Refused int64 `json:"refused"`
	// the network's online providers on an older release or with signing off
	StillToUpdate int64 `json:"still_to_update"`
	// the network has had its one top-up
	TopUpUsed bool `json:"top_up_used"`
}

type SnFleetClaimCallback connect.ApiCallback[*SnFleetClaimState]

// What paying for a registration takes right now, read together.
type snFleetBurnPlan struct {
	BurnRao uint64
	FeeRao  uint64
	// the key holds the burn and fee and would keep its deposit
	Covered bool
	// a sale of SN25α that pays for it, nil when none can. A sale has to pay the
	// runtime's minimum, so a key with little alpha can't convert just the burn.
	Convert *BittensorConvertEstimateResult
}

// snFleetClaimChain is what the flow needs from the chain. Each write builds one
// fixed call from typed values: nothing here can sign a call the server chose.
type snFleetClaimChain interface {
	Block(ctx context.Context) (uint64, error)
	HotkeyUid(ctx context.Context, netuid uint16, hotkey [32]byte) (uid int64, registered bool, err error)
	BurnPlan(ctx context.Context, coldkey [32]byte) (*snFleetBurnPlan, error)
	// the hotkey's published fleet commitment
	FleetCommitment(ctx context.Context, netuid uint16, hotkey [32]byte) (hash [32]byte, found bool, err error)
	// the plan's sale, with its price limit, to the key that signs
	ConvertForBurn(ctx context.Context, coldkeyPhrase string, plan *snFleetBurnPlan) error
	// register_limit, signed by the coldkey
	RegisterLimit(ctx context.Context, coldkeyPhrase string, netuid uint16, hotkey [32]byte, limitRao uint64) error
	// set_commitment of the tagged fleet format, signed by the hotkey
	SetFleetCommitment(ctx context.Context, hotkeySeed [32]byte, netuid uint16, hash [32]byte) error
}

// snFleetClaimColdkey is a recovery phrase's two operations.
type snFleetClaimColdkey interface {
	Address(phrase string) (string, error)
	SignMessage(phrase string, message string) (string, error)
}

// SnFleetClaim is the flow for one network. Safe for concurrent use: one step runs
// at a time.
type SnFleetClaim struct {
	server  snFleetServer
	store   SnFleetClaimStore
	chain   snFleetClaimChain
	coldkey snFleetClaimColdkey
	random  func(out []byte) error

	stateLock sync.Mutex
}

// NewSnFleetClaim is the flow on this device, using its api, the chain settings and
// the app's store.
func NewSnFleetClaim(api *Api, settings *SnChainSettings, store SnFleetClaimStore) *SnFleetClaim {
	return newSnFleetClaim(api, store, newSnFleetClaimEngineChain(settings), snFleetClaimLocalColdkey{})
}

func newSnFleetClaim(server snFleetServer, store SnFleetClaimStore, chain snFleetClaimChain, coldkey snFleetClaimColdkey) *SnFleetClaim {
	return &SnFleetClaim{
		server:  server,
		store:   store,
		chain:   chain,
		coldkey: coldkey,
		random: func(out []byte) error {
			_, err := rand.Read(out)
			return err
		},
	}
}

// The next thing to report. Lets a step say "stop here".
func snFleetClaimFail(state *SnFleetClaimState, code string, message string) *SnFleetClaimState {
	state.Stage = SnFleetClaimStageFailed
	state.ErrorCode = code
	state.ErrorMessage = message
	state.RetryAfterSeconds = snFleetClaimRetryLong
	return state
}

// Tap starts or resumes the whole claim, with leave to spend: one authorization,
// one conversion or top-up, one registration.
func (self *SnFleetClaim) Tap(callback SnFleetClaimCallback) {
	self.run(callback, func(ctx context.Context) *SnFleetClaimState { return self.TapSync(ctx) })
}

// Advance does what needs no tap, whenever the app is running: publishing and
// signing, so renewing and adding devices just happen. It spends only to finish a
// first claim that was already tapped.
func (self *SnFleetClaim) Advance(callback SnFleetClaimCallback) {
	self.run(callback, func(ctx context.Context) *SnFleetClaimState { return self.AdvanceSync(ctx) })
}

// Refresh reads where the claim stands, and signs and sends nothing.
func (self *SnFleetClaim) Refresh(callback SnFleetClaimCallback) {
	self.run(callback, func(ctx context.Context) *SnFleetClaimState { return self.RefreshSync(ctx) })
}

// SubmitWalletAuthorization takes the wallet's signature over WalletMessage, checks
// it, sends it and goes on as Tap does.
func (self *SnFleetClaim) SubmitWalletAuthorization(signature string, callback SnFleetClaimCallback) {
	self.run(callback, func(ctx context.Context) *SnFleetClaimState {
		return self.SubmitWalletAuthorizationSync(ctx, signature)
	})
}

func (self *SnFleetClaim) run(callback SnFleetClaimCallback, step func(ctx context.Context) *SnFleetClaimState) {
	go connect.HandleError(func() {
		ctx, cancel := context.WithTimeout(context.Background(), snFleetClaimStepTimeout)
		defer cancel()
		state := step(ctx)
		if callback != nil {
			callback.Result(state, nil)
		}
	})
}

//gomobile:noexport
func (self *SnFleetClaim) TapSync(ctx context.Context) *SnFleetClaimState {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.step(ctx, snFleetClaimModeTap, "")
}

//gomobile:noexport
func (self *SnFleetClaim) AdvanceSync(ctx context.Context) *SnFleetClaimState {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.step(ctx, snFleetClaimModeAdvance, "")
}

//gomobile:noexport
func (self *SnFleetClaim) RefreshSync(ctx context.Context) *SnFleetClaimState {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.step(ctx, snFleetClaimModeRefresh, "")
}

//gomobile:noexport
func (self *SnFleetClaim) SubmitWalletAuthorizationSync(ctx context.Context, signature string) *SnFleetClaimState {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if strings.TrimSpace(signature) == "" {
		return snFleetClaimFail(&SnFleetClaimState{}, SnFleetClaimErrorWalletSignature, "The wallet returned no signature.")
	}
	return self.step(ctx, snFleetClaimModeTap, signature)
}

type snFleetClaimMode int

const (
	// reads only
	snFleetClaimModeRefresh snFleetClaimMode = iota
	// free steps only
	snFleetClaimModeAdvance
	// everything
	snFleetClaimModeTap
)

func snFleetClaimManifestLimits(manifest *protocol.FleetManifest, hotkey [32]byte, authorization *protocol.FleetHotkeyAuthorization) error {
	if manifest.Hotkey != hotkey {
		return errors.New("the manifest names another hotkey")
	}
	if manifest.ChainID != authorization.ChainID || manifest.Netuid != authorization.Netuid || manifest.Coordinator != authorization.Coordinator {
		return errors.New("the manifest names another deployment")
	}
	if protocol.FleetRelayDeviceLimit < len(manifest.Members) {
		return fmt.Errorf("the manifest has %d members, more than the %d a one-tap fleet can have", len(manifest.Members), protocol.FleetRelayDeviceLimit)
	}
	return nil
}

// snFleetClaimSignWork turns the server's list into hotkey signatures. All or
// nothing: anything off in one binding means the server isn't to be trusted with
// the others either. Each binding is rebuilt from the manifest, the hotkey signs
// that, and it signs only a window the coordinator could accept.
func snFleetClaimSignWork(manifest *protocol.FleetManifest, seed [32]byte, work []*snFleetHotkeyWork, currentEpoch uint64) ([]*snFleetHotkeySignature, error) {
	signatures := make([]*snFleetHotkeySignature, 0, len(work))
	for _, item := range work {
		if item == nil {
			return nil, errors.New("the server listed an empty binding")
		}
		binding, err := snParseFleetBinding(item.BindingJson)
		if err != nil {
			return nil, err
		}
		member := protocol.FleetMember{ClientID: binding.ClientID, ClientKey: binding.ClientKey}
		rebuilt, err := manifest.Binding(member, binding.ValidFromEpoch, binding.ValidToEpoch)
		if err != nil {
			return nil, err
		}
		if rebuilt != *binding {
			return nil, errors.New("the binding differs from the one its manifest makes")
		}
		if binding.ValidFromEpoch <= currentEpoch {
			return nil, fmt.Errorf("the binding starts at epoch %d, which has already begun", binding.ValidFromEpoch)
		}
		if protocol.FleetBindingMaxEpochs < binding.ValidToEpoch-binding.ValidFromEpoch+1 {
			return nil, fmt.Errorf("the binding lasts longer than %d epochs", protocol.FleetBindingMaxEpochs)
		}
		signature, err := protocol.SignFleetBindingWithHotkey(seed, rebuilt)
		if err != nil {
			return nil, err
		}
		digest, err := rebuilt.Digest()
		if err != nil {
			return nil, err
		}
		signatures = append(signatures, &snFleetHotkeySignature{Digest: snFleetHex(digest[:]), HotkeySignature: snFleetHex(signature)})
	}
	return signatures, nil
}

// snFleetClaimCoordinator reads the deployment the server states.
func snFleetClaimDeployment(status *snFleetClaimStatus) (chainId uint64, netuid uint16, coordinator [20]byte, err error) {
	if status.ChainId <= 0 || status.Netuid <= 0 || 0xffff < status.Netuid {
		return 0, 0, coordinator, errors.New("the server states no deployment")
	}
	raw, err := evm.ParseHexBytes(status.Coordinator)
	if err != nil || len(raw) != len(coordinator) {
		return 0, 0, coordinator, errors.New("the server states no coordinator")
	}
	copy(coordinator[:], raw)
	return uint64(status.ChainId), uint16(status.Netuid), coordinator, nil
}

// The one pass. It stops at the first thing that needs someone or something else,
// and says what that is.
func (self *SnFleetClaim) step(ctx context.Context, mode snFleetClaimMode, walletSignature string) *SnFleetClaimState {
	state := &SnFleetClaimState{}
	status, err := self.server.snFleetClaimStatus(ctx)
	if err == nil && status == nil {
		err = errSnFleetNoResult
	}
	if err != nil {
		return snFleetClaimFail(state, SnFleetClaimErrorServer, "The claim's state can't be read right now.")
	}
	state.ClaimPrompt = status.ClaimPrompt
	state.Netuid = status.Netuid
	state.CurrentEpoch = status.CurrentEpoch
	state.Chosen, state.Ready, state.Bound = status.Counts.Chosen, status.Counts.Ready, status.Counts.Bound
	state.Refused, state.StillToUpdate = status.Counts.Refused, status.Counts.StillToUpdate
	state.Registered, state.Uid = status.Registered, status.Uid
	state.TopUpUsed = status.TopUp.Used
	if !status.BindingOpen {
		state.Stage = SnFleetClaimStageClosed
		return state
	}
	if status.ConsentColdkey == "" {
		state.Stage = SnFleetClaimStageNoConsent
		return state
	}
	state.ColdkeyAddress = status.ConsentColdkey
	consentColdkey, err := ss58.DecodeWithPrefix(status.ConsentColdkey, SnSs58Prefix)
	if err != nil {
		return snFleetClaimFail(state, SnFleetClaimErrorServer, "The server names a malformed coldkey.")
	}
	networkId, ok := credentialNetworkId(self.server.snFleetByJwt())
	if !ok {
		return snFleetClaimFail(state, SnFleetClaimErrorServer, "This session names no network.")
	}
	chainId, netuid, coordinator, err := snFleetClaimDeployment(status)
	if err != nil {
		return snFleetClaimFail(state, SnFleetClaimErrorServer, "The server states no deployment.")
	}

	var authorized *protocol.FleetHotkeyAuthorization
	if status.AuthorizationMessage != "" {
		authorized, err = protocol.ParseFleetHotkeyAuthorization(status.AuthorizationMessage)
		if err != nil || authorized.NetworkID != [16]byte(networkId) {
			return snFleetClaimFail(state, SnFleetClaimErrorServer, "The server holds an authorization this device can't read.")
		}
		state.HotkeyAddress, _ = ss58.Encode(authorized.Hotkey, SnSs58Prefix)
	}

	// the hotkey
	seed, hotkey, hasHotkey, err := self.loadHotkey(networkId)
	if err != nil {
		return snFleetClaimFail(state, SnFleetClaimErrorSecretStore, "The fleet hotkey can't be read from this device.")
	}
	if !hasHotkey {
		if authorized != nil && authorized.Coldkey == consentColdkey {
			state.Stage = SnFleetClaimStageHotkeyElsewhere
			return state
		}
		if mode != snFleetClaimModeTap {
			state.Stage = SnFleetClaimStageNeedsTap
			return state
		}
		seed, hotkey, err = self.createHotkey(networkId)
		if err != nil {
			return snFleetClaimFail(state, SnFleetClaimErrorSecretStore, "The fleet hotkey couldn't be saved on this device, so nothing was signed.")
		}
	}
	state.HotkeyAddress, _ = ss58.Encode(hotkey, SnSs58Prefix)
	if authorized != nil && authorized.Coldkey == consentColdkey && authorized.Hotkey != hotkey {
		// another device's hotkey. Replacing it would cost a new UID and stop every
		// device's auto-signing, so that isn't what a tap means here
		state.HotkeyAddress, _ = ss58.Encode(authorized.Hotkey, SnSs58Prefix)
		state.Stage = SnFleetClaimStageHotkeyElsewhere
		return state
	}

	phrase := self.store.LoadSnFleetColdkeyPhrase(networkId.String())
	if phrase != "" {
		address, err := self.coldkey.Address(phrase)
		if err != nil {
			return snFleetClaimFail(state, SnFleetClaimErrorSecretStore, "The recovery phrase on this device can't be read.")
		}
		if address != status.ConsentColdkey {
			// the payout wallet was replaced: this phrase no longer speaks for it
			phrase = ""
		}
	}

	// the authorization
	if authorized == nil || authorized.Coldkey != consentColdkey {
		if mode != snFleetClaimModeTap {
			state.Stage = SnFleetClaimStageNeedsTap
			return state
		}
		message, err := protocol.FleetHotkeyAuthorization{ChainID: chainId, Netuid: netuid, Coordinator: coordinator, NetworkID: [16]byte(networkId), Coldkey: consentColdkey, Hotkey: hotkey}.Message()
		if err != nil {
			return snFleetClaimFail(state, SnFleetClaimErrorServer, "The authorization can't be built for this deployment.")
		}
		signature := walletSignature
		if signature == "" {
			if phrase == "" {
				state.Stage = SnFleetClaimStageNeedsWalletAuthorization
				state.WalletMessage = message
				return state
			}
			signature, err = self.coldkey.SignMessage(phrase, message)
			if err != nil {
				return snFleetClaimFail(state, SnFleetClaimErrorSecretStore, "The recovery phrase on this device can't sign.")
			}
		}
		raw, err := snFleetParseHex(NormalizeBittensorSignature(signature))
		if err != nil || !protocol.VerifyWalletSignature(consentColdkey, message, raw) {
			return snFleetClaimFail(state, SnFleetClaimErrorWalletSignature, "That signature isn't this network's coldkey signing this authorization.")
		}
		accepted, err := self.server.snFleetAuthorize(ctx, &snFleetAuthorizationArgs{Message: message, Signature: snFleetHex(raw)})
		if err != nil || accepted == nil {
			return snFleetClaimFail(state, SnFleetClaimErrorServer, "The authorization couldn't be sent. Try again.")
		}
		if accepted.Error != nil || !accepted.Accepted {
			return snFleetClaimFail(state, SnFleetClaimErrorAuthorizationRefused, "The server refused the authorization.")
		}
		authorized, err = protocol.ParseFleetHotkeyAuthorization(message)
		if err != nil {
			return snFleetClaimFail(state, SnFleetClaimErrorServer, "The authorization can't be read.")
		}
	}

	// the registration. The server's census saying yes is enough to go on, since what
	// follows is free and the chain refuses it otherwise. Its no is never enough to
	// spend: the chain has to say so too
	if !status.Registered && !hasRegisteredHotkey(ctx, self.chain, netuid, hotkey, state) {
		if state.Stage != "" {
			return state
		}
		// an authorization with no manifest yet is a first claim someone tapped.
		// Once a manifest exists, this UID was lost, and a new one spends again
		firstClaim := mode == snFleetClaimModeAdvance && status.Manifest == nil
		if mode != snFleetClaimModeTap && !firstClaim {
			state.Stage = SnFleetClaimStageNeedsTap
			return state
		}
		return self.register(ctx, status, netuid, consentColdkey, hotkey, phrase, state)
	}

	// what's published and signed from here on needs only the hotkey, and is free
	if status.Manifest == nil {
		state.Stage = SnFleetClaimStageChoosingDevices
		state.RetryAfterSeconds = snFleetClaimRetryLong
		return state
	}
	manifest, err := protocol.ParseFleetManifest([]byte(status.Manifest.Manifest))
	if err == nil {
		err = snFleetClaimManifestLimits(manifest, hotkey, authorized)
	}
	if err != nil {
		return snFleetClaimFail(state, SnFleetClaimErrorBadManifest, "The server's device list can't be signed, so nothing was.")
	}
	if mode == snFleetClaimModeRefresh {
		state.Stage = snFleetClaimWaitStage(status)
		state.RetryAfterSeconds = snFleetClaimRetryLong
		return state
	}
	hash, err := manifest.CommitmentHash()
	if err != nil {
		return snFleetClaimFail(state, SnFleetClaimErrorBadManifest, "The server's device list can't be signed, so nothing was.")
	}
	if !status.Manifest.Mirrored {
		published, found, err := self.chain.FleetCommitment(ctx, netuid, hotkey)
		if err != nil {
			return snFleetClaimFail(state, SnFleetClaimErrorChain, "The chain can't be read right now.")
		}
		if !found || published != hash {
			if err := self.chain.SetFleetCommitment(ctx, seed, netuid, hash); err != nil {
				return snFleetClaimFail(state, SnFleetClaimErrorChain, "The commitment couldn't be published. Try again.")
			}
		}
	}
	if 0 < len(status.HotkeyWork) {
		signatures, err := snFleetClaimSignWork(manifest, seed, status.HotkeyWork, uint64(status.CurrentEpoch))
		if err != nil {
			return snFleetClaimFail(state, SnFleetClaimErrorBadBinding, "A binding the server sent can't be signed, so none were.")
		}
		sent, err := self.server.snFleetHotkeySignatures(ctx, &snFleetHotkeySignaturesArgs{Signatures: signatures})
		if err != nil || sent == nil || sent.Error != nil {
			return snFleetClaimFail(state, SnFleetClaimErrorServer, "The signatures couldn't be sent. Try again.")
		}
	}
	state.Stage = snFleetClaimWaitStage(status)
	if state.Stage != SnFleetClaimStageBound {
		state.RetryAfterSeconds = snFleetClaimRetryShort
	}
	return state
}

func snFleetClaimWaitStage(status *snFleetClaimStatus) string {
	switch {
	case status.Manifest != nil && !status.Manifest.Mirrored:
		return SnFleetClaimStageWaitingMirror
	case 0 < status.Counts.Chosen && status.Counts.Bound == status.Counts.Chosen:
		return SnFleetClaimStageBound
	default:
		return SnFleetClaimStageWaitingDevices
	}
}

// Whether the chain says the hotkey is registered. A read failure fills the state,
// which stops the flow: a registration must never follow a failed read.
func hasRegisteredHotkey(ctx context.Context, chain snFleetClaimChain, netuid uint16, hotkey [32]byte, state *SnFleetClaimState) bool {
	uid, registered, err := chain.HotkeyUid(ctx, netuid, hotkey)
	if err != nil {
		snFleetClaimFail(state, SnFleetClaimErrorChain, "The chain can't be read right now.")
		return false
	}
	state.Registered = registered
	state.Uid = uid
	return registered
}

// Register the hotkey, once there's enough to pay for it: step 1 and step 2 of the
// plan's table.
func (self *SnFleetClaim) register(ctx context.Context, status *snFleetClaimStatus, netuid uint16, coldkey [32]byte, hotkey [32]byte, phrase string, state *SnFleetClaimState) *SnFleetClaimState {
	chainFail := func() *SnFleetClaimState {
		return snFleetClaimFail(state, SnFleetClaimErrorChain, "The chain can't be read right now.")
	}
	block, err := self.chain.Block(ctx)
	if err != nil {
		return chainFail()
	}
	// a new UID is immune for 3 days and can then be replaced, and its binding can
	// be up to 7 days away, so the server names when registering is safe
	if int64(block) < status.RegisterAfterBlock || status.RegisterBeforeBlock < int64(block) {
		state.Stage = SnFleetClaimStageWaitingRegisterWindow
		state.RegisterAfterBlock = status.RegisterAfterBlock
		state.RetryAfterSeconds = snFleetClaimRetryLong
		return state
	}
	plan, err := self.chain.BurnPlan(ctx, coldkey)
	if err != nil {
		return chainFail()
	}
	if uint64(SnFleetClaimMaxBurnRao) < plan.BurnRao {
		state.Stage = SnFleetClaimStageWaitingBurn
		state.RetryAfterSeconds = snFleetClaimRetryLong
		return state
	}
	burn := plan.BurnRao
	state.BurnLimitRao = int64(burn)
	if !plan.Covered {
		// converting first. A sale has to pay the runtime's minimum (0.002 TAO), which
		// is several times the burn, so a key with a little alpha can't: it gets the
		// one top-up rather than a failure
		if phrase != "" && plan.Convert != nil {
			if err := self.chain.ConvertForBurn(ctx, phrase, plan); err != nil {
				return snFleetClaimFail(state, SnFleetClaimErrorChain, "The conversion couldn't be sent. Try again.")
			}
			state.Stage = SnFleetClaimStageConverting
			state.RetryAfterSeconds = snFleetClaimRetryShort
			return state
		}
		switch {
		case status.TopUp.Used && !status.TopUp.Landed:
			state.Stage = SnFleetClaimStageWaitingTopUp
			state.RetryAfterSeconds = snFleetClaimRetryShort
			return state
		case status.TopUp.Used:
			// it paid the burn of its moment, and the burn has gone up since. It falls
			// back about every 72 minutes
			state.Stage = SnFleetClaimStageWaitingBurn
			state.RetryAfterSeconds = snFleetClaimRetryLong
			return state
		case status.TopUp.Available:
			result, err := self.server.snFleetTopUp(ctx)
			if err != nil || result == nil {
				return snFleetClaimFail(state, SnFleetClaimErrorServer, "The top-up couldn't be requested. Try again.")
			}
			if result.Error != nil {
				return snFleetClaimFail(state, SnFleetClaimErrorTopUpRefused, "The top-up isn't available right now.")
			}
			state.TopUpUsed = true
			state.Stage = SnFleetClaimStageWaitingTopUp
			state.RetryAfterSeconds = snFleetClaimRetryShort
			return state
		default:
			state.Stage = SnFleetClaimStageNeedsFunds
			return state
		}
	}
	if phrase == "" {
		state.Stage = SnFleetClaimStageNeedsWalletRegistration
		state.RetryAfterSeconds = snFleetClaimRetryShort
		return state
	}
	if err := self.chain.RegisterLimit(ctx, phrase, netuid, hotkey, burn); err != nil {
		return snFleetClaimFail(state, SnFleetClaimErrorChain, "The registration couldn't be sent. Try again.")
	}
	state.Stage = SnFleetClaimStageRegistering
	state.RetryAfterSeconds = snFleetClaimRetryShort
	return state
}

// The stored hotkey. An empty answer is a device with none; anything else that
// isn't a seed stops the flow, or a failed read would make a second hotkey.
func (self *SnFleetClaim) loadHotkey(networkId connect.Id) (seed [32]byte, hotkey [32]byte, found bool, err error) {
	saved := self.store.LoadSnFleetHotkeySeed(networkId.String())
	if saved == "" {
		return seed, hotkey, false, nil
	}
	raw, err := hex.DecodeString(saved)
	if err != nil || len(raw) != len(seed) {
		return seed, hotkey, false, errors.New("the saved hotkey seed can't be read")
	}
	copy(seed[:], raw)
	hotkey, err = protocol.FleetHotkeyPublicKey(seed)
	if err != nil {
		return seed, hotkey, false, err
	}
	return seed, hotkey, true, nil
}

// A new hotkey: separate from the recovery phrase, stored and read back before
// anything is signed with it, since a fleet nobody holds the key of can't renew.
func (self *SnFleetClaim) createHotkey(networkId connect.Id) (seed [32]byte, hotkey [32]byte, err error) {
	if err := self.random(seed[:]); err != nil {
		return seed, hotkey, errors.New("no randomness for a new hotkey")
	}
	seedHex := hex.EncodeToString(seed[:])
	if !self.store.SaveSnFleetHotkeySeed(networkId.String(), seedHex) {
		return seed, hotkey, errors.New("the hotkey could not be saved")
	}
	if self.store.LoadSnFleetHotkeySeed(networkId.String()) != seedHex {
		return seed, hotkey, errors.New("the hotkey did not read back")
	}
	hotkey, err = protocol.FleetHotkeyPublicKey(seed)
	return seed, hotkey, err
}
