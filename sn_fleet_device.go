//go:build !ios_extension && !js

package sdk

// A device's half of a Top 200 claim, the one copy `provider` and the apps share
// (apple BTEASY.md, "What has to hold" and "Headless miners").
//
// Signing a binding takes this device out of the earnings pool from the binding's
// first epoch and puts it in a fleet whose hotkey another key holds. A person used
// to decide that. Here no person looks, so the device checks for itself that the
// binding names its own client id and key, carries a valid hotkey signature, and
// names a hotkey the network's coldkey authorized. It pins that coldkey when it
// first sees the network's consent, before there is anything to attack, and pins
// the hotkey when it first sees one authorized. A changed coldkey or hotkey stops
// signing and says why. The server can't change any of that: it chooses devices
// and carries messages.
//
// Everything a device received and signed is saved through SnFleetDeviceStore
// before its signature leaves, so a restart mid-binding sends the same signature
// again rather than signing twice.
//
// The server decides when to ask again, within bounds this code holds. A check
// costs the server almost nothing, since it answers from its short list of
// networks, and the check is slow and jittered because every device asks.

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk/sn/protocol"
)

// Why a device refused a binding, in SnFleetDeviceCheckResult.RefusalCode.
const (
	SnFleetRefusalOtherClient        = protocol.FleetRefusalOtherClient
	SnFleetRefusalOtherClientKey     = protocol.FleetRefusalOtherClientKey
	SnFleetRefusalBadBinding         = protocol.FleetRefusalBadBinding
	SnFleetRefusalBadHotkeySignature = protocol.FleetRefusalBadHotkeySignature
	SnFleetRefusalColdkeyNotPinned   = protocol.FleetRefusalColdkeyNotPinned
	SnFleetRefusalBadConsent         = protocol.FleetRefusalBadConsent
	SnFleetRefusalColdkeyChanged     = protocol.FleetRefusalColdkeyChanged
	SnFleetRefusalHotkeyChanged      = protocol.FleetRefusalHotkeyChanged
	SnFleetRefusalBadAuthorization   = protocol.FleetRefusalBadAuthorization
	SnFleetRefusalNotAuthorized      = protocol.FleetRefusalNotAuthorized
	SnFleetRefusalGenerationReplayed = protocol.FleetRefusalGenerationReplayed
	SnFleetRefusalLeft               = protocol.FleetRefusalLeft
	SnFleetRefusalStateUnreadable    = protocol.FleetRefusalStateUnreadable
)

// What one check did, in SnFleetDeviceCheckResult.Action.
const (
	// nothing asked of this device
	SnFleetDeviceActionNone = "none"
	// first sight of the network's consent: the coldkey is pinned
	SnFleetDeviceActionPinned = "pinned"
	// a binding checked, signed, saved and sent
	SnFleetDeviceActionSigned = "signed"
	// a binding this device already signed, sent again without signing
	SnFleetDeviceActionResent  = "resent"
	SnFleetDeviceActionRefused = "refused"
)

const (
	snFleetDeviceStateSchema = "urnetwork-fleet-device-state-v1"
	// how many signed bindings a device keeps, newest last: a binding lasts up
	// to 12 epochs, so this reaches back years
	snFleetDeviceMaxSigned = 8

	// when there is no answer to go by: the server hasn't this route yet, the
	// network is down, or the request failed
	snFleetDeviceCheckDefault = 24 * time.Hour
	// no server answer moves a check outside these
	snFleetDeviceCheckMin = 10 * time.Minute
	// a week and a day: while binding is closed the server asks for a week
	snFleetDeviceCheckMax = 8 * 24 * time.Hour
	// a run that finds its saved check overdue makes it within this, spread out
	snFleetDeviceCheckOverdue = time.Hour
	// a server without the route yet: it gets one when binding is about to open, and
	// asking daily until then would only be load
	snFleetDeviceCheckNoRoute = 7 * 24 * time.Hour

	snFleetDeviceCheckTimeout = 2 * time.Minute
)

// SnFleetDeviceStore is where a device saves what it pinned, received and signed.
// The app's store, or a file in `provider`'s state directory. It must survive a
// restart, and it must not be readable by another network's session.
type SnFleetDeviceStore interface {
	// the saved text, "" when nothing has ever been saved. On a read failure,
	// any other text: it fails to parse and stops signing, where a "" would pin
	// the next coldkey as if this were a new device.
	LoadSnFleetDeviceState() string
	// true once the text is durably saved
	SaveSnFleetDeviceState(stateJson string) bool
}

// What one check did.
type SnFleetDeviceCheckResult struct {
	Action string `json:"action"`
	// with SnFleetDeviceActionRefused, one of the SnFleetRefusal codes
	RefusalCode string `json:"refusal_code,omitempty"`
	// why, in a sentence for a log or a screen
	RefusalMessage string `json:"refusal_message,omitempty"`
	// the binding's generation, 0 when there was none
	Generation  int64 `json:"generation,omitempty"`
	BindingOpen bool  `json:"binding_open"`
	// when to check next, jittered
	NextCheckSeconds int64 `json:"next_check_seconds"`
	// the server didn't take the signature; the next check sends it again
	SendFailed bool `json:"send_failed,omitempty"`
}

// How a Leave went.
type SnFleetDeviceLeaveOutcome struct {
	// the server took it. A device that was never bound has nothing on chain
	Left bool `json:"left"`
	// the first epoch this device is back in the pool; 0 when it was never bound
	EffectiveEpoch int64 `json:"effective_epoch"`
	// the id of the string the Leave screen shows (sn_fleet_disclaimer.go). A device
	// leaving on its own holds no key of the claim; SnFleetClaim.Leave says if it does
	FundsDisclaimer string `json:"funds_disclaimer"`
}

type SnFleetDeviceCheckCallback connect.ApiCallback[*SnFleetDeviceCheckResult]

type SnFleetDeviceLeaveCallback connect.ApiCallback[*SnFleetDeviceLeaveOutcome]

// SnFleetDeviceListener hears each background check of SnFleetDevice.Start.
type SnFleetDeviceListener interface {
	SnFleetDeviceChecked(result *SnFleetDeviceCheckResult)
}

// One binding this device signed.
type snFleetSignedBinding struct {
	Digest          string                `json:"digest"`
	Generation      int64                 `json:"generation"`
	BindingJson     string                `json:"binding_json"`
	HotkeySignature string                `json:"hotkey_signature"`
	ClientSignature string                `json:"client_signature"`
	Authorization   *snFleetSignedMessage `json:"authorization"`
	SignedMillis    int64                 `json:"signed_millis"`
}

// What a device remembers. Keys are 0x hex.
type snFleetDeviceState struct {
	Schema    string `json:"schema"`
	NetworkId string `json:"network_id"`
	ClientId  string `json:"client_id"`
	// the coldkey of the network's consent when this device first saw one
	Coldkey           string `json:"coldkey,omitempty"`
	ConsentGeneration int64  `json:"consent_generation,omitempty"`
	// the hotkey that coldkey first authorized
	Hotkey string `json:"hotkey,omitempty"`
	// newest last
	Signed []*snFleetSignedBinding `json:"signed,omitempty"`
	// this device left; it signs nothing until someone resets it
	Left bool `json:"left,omitempty"`
	// when the next check is due, so a restart doesn't ask sooner than the server said
	NextCheckMillis int64 `json:"next_check_millis,omitempty"`
}

func snFleetHex(b []byte) string {
	return "0x" + hex.EncodeToString(b)
}

func snFleetParseHex(value string) ([]byte, error) {
	return hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(value), "0x"))
}

// The saved state, or a new one for this identity. Text that isn't a state is an
// error: the device can't tell what it pinned, and a new state would pin whatever
// arrives next. A state of another identity, when a host signs in somewhere else,
// is not this device's history.
func snFleetLoadDeviceState(saved string, networkId connect.Id, clientId connect.Id) (*snFleetDeviceState, error) {
	fresh := &snFleetDeviceState{Schema: snFleetDeviceStateSchema, NetworkId: networkId.String(), ClientId: clientId.String()}
	if saved == "" {
		return fresh, nil
	}
	state := &snFleetDeviceState{}
	if err := json.Unmarshal([]byte(saved), state); err != nil {
		return nil, errors.New("the saved fleet state can't be read")
	}
	if state.Schema != snFleetDeviceStateSchema {
		return nil, errors.New("the saved fleet state has another format")
	}
	if state.NetworkId != networkId.String() || state.ClientId != clientId.String() {
		return fresh, nil
	}
	return state, nil
}

func snFleetDeviceRefusal(code string, format string, args ...any) *SnFleetDeviceCheckResult {
	return &SnFleetDeviceCheckResult{Action: SnFleetDeviceActionRefused, RefusalCode: code, RefusalMessage: fmt.Sprintf(format, args...)}
}

// What a consent check decided.
func snFleetPinConsent(state *snFleetDeviceState, networkId connect.Id, consent *snFleetSignedMessage) (*SnFleetDeviceCheckResult, bool) {
	statement, err := decodeSnNetworkConsentStatement(consent.Message)
	if err != nil {
		return snFleetDeviceRefusal(SnFleetRefusalBadConsent, "The network's consent can't be read."), false
	}
	if statement.NetworkId != [16]byte(networkId) {
		return snFleetDeviceRefusal(SnFleetRefusalBadConsent, "The consent names another network."), false
	}
	signature, err := snFleetParseHex(consent.Signature)
	if err != nil || !protocol.VerifyWalletSignature(statement.Coldkey, consent.Message, signature) {
		return snFleetDeviceRefusal(SnFleetRefusalBadConsent, "The network's consent isn't signed by its coldkey."), false
	}
	coldkey := snFleetHex(statement.Coldkey[:])
	if state.Coldkey == "" {
		state.Coldkey = coldkey
		state.ConsentGeneration = int64(statement.Generation)
		return nil, true
	}
	if state.Coldkey != coldkey {
		return snFleetDeviceRefusal(SnFleetRefusalColdkeyChanged, "The network's coldkey changed from %s to %s, so this device stopped signing. If that was you, run the reset for this device.", state.Coldkey, coldkey), false
	}
	if state.ConsentGeneration < int64(statement.Generation) {
		state.ConsentGeneration = int64(statement.Generation)
		return nil, true
	}
	return nil, false
}

// A decision about one piece of work. It signs nothing itself: the caller saves
// the state first.
type snFleetBindingDecision struct {
	refusal *SnFleetDeviceCheckResult
	// the checked binding, nil when none was offered or it was refused
	binding *protocol.FleetBinding
	digest  string
	// a binding this device has already signed
	signed *snFleetSignedBinding
}

// snFleetCheckBinding runs every check a device owes a binding, in the order that
// says the most useful thing when several fail. The state's hotkey pin is set only
// when the binding passes all of them, so a refused binding pins nothing.
func snFleetCheckBinding(state *snFleetDeviceState, networkId connect.Id, clientId connect.Id, clientKey []byte, work *snFleetDeviceWork) *snFleetBindingDecision {
	refuse := func(code string, format string, args ...any) *snFleetBindingDecision {
		return &snFleetBindingDecision{refusal: snFleetDeviceRefusal(code, format, args...)}
	}
	if state.Left {
		return refuse(SnFleetRefusalLeft, "This device left the fleet, so it signs nothing until it's reset.")
	}
	binding, err := snParseFleetBinding(work.BindingJson)
	if err != nil {
		return refuse(SnFleetRefusalBadBinding, "The binding can't be read: %s.", err)
	}
	digest, err := binding.Digest()
	if err != nil {
		return refuse(SnFleetRefusalBadBinding, "The binding can't be read: %s.", err)
	}
	decision := &snFleetBindingDecision{digest: snFleetHex(digest[:])}
	refused := func(code string, format string, args ...any) *snFleetBindingDecision {
		decision.refusal = snFleetDeviceRefusal(code, format, args...)
		decision.refusal.Generation = int64(binding.Generation)
		return decision
	}
	if binding.ClientID != [16]byte(clientId) {
		return refused(SnFleetRefusalOtherClient, "The binding names another client, %s.", connect.Id(binding.ClientID))
	}
	if len(clientKey) != ed25519.PublicKeySize || string(binding.ClientKey[:]) != string(clientKey) {
		return refused(SnFleetRefusalOtherClientKey, "The binding names another client key, %s.", snFleetHex(binding.ClientKey[:]))
	}
	if state.Coldkey == "" {
		return refused(SnFleetRefusalColdkeyNotPinned, "This device hasn't seen its network's consent, so it has no coldkey to trust.")
	}
	if work.Authorization == nil {
		return refused(SnFleetRefusalBadAuthorization, "The binding comes with no authorization from the network's coldkey.")
	}
	authorizationSignature, err := snFleetParseHex(work.Authorization.Signature)
	if err != nil {
		return refused(SnFleetRefusalBadAuthorization, "The authorization's signature can't be read.")
	}
	authorization, err := protocol.VerifyFleetHotkeyAuthorization(work.Authorization.Message, authorizationSignature)
	if err != nil {
		return refused(SnFleetRefusalBadAuthorization, "The hotkey's authorization doesn't verify: %s.", err)
	}
	if authorization.NetworkID != [16]byte(networkId) {
		return refused(SnFleetRefusalBadAuthorization, "The authorization names another network.")
	}
	coldkey := snFleetHex(authorization.Coldkey[:])
	if coldkey != state.Coldkey {
		return refused(SnFleetRefusalColdkeyChanged, "The hotkey is authorized by %s, but this device pinned the coldkey %s, so it stopped signing. If the network changed its coldkey, run the reset for this device.", coldkey, state.Coldkey)
	}
	if authorization.Hotkey != binding.Hotkey || authorization.ChainID != binding.ChainID || authorization.Netuid != binding.Netuid || authorization.Coordinator != binding.Coordinator {
		return refused(SnFleetRefusalNotAuthorized, "The binding's hotkey or deployment isn't the one the coldkey authorized.")
	}
	hotkey := snFleetHex(binding.Hotkey[:])
	if state.Hotkey != "" && state.Hotkey != hotkey {
		return refused(SnFleetRefusalHotkeyChanged, "The fleet's hotkey changed from %s to %s, so this device stopped signing. If that was you, run the reset for this device.", state.Hotkey, hotkey)
	}
	hotkeySignature, err := snFleetParseHex(work.HotkeySignature)
	if err != nil || !binding.VerifyHotkey(hotkeySignature) {
		return refused(SnFleetRefusalBadHotkeySignature, "The hotkey's signature over the binding doesn't verify.")
	}
	for _, signed := range state.Signed {
		if signed.Digest == decision.digest {
			decision.binding = binding
			decision.signed = signed
			return decision
		}
	}
	for _, signed := range state.Signed {
		// generations only go up: this device can't have two bindings of one
		// generation, or go back to an older one
		if uint64(signed.Generation) >= binding.Generation {
			return refused(SnFleetRefusalGenerationReplayed, "This device already signed generation %d, so it won't sign a different binding for generation %d.", signed.Generation, binding.Generation)
		}
	}
	decision.binding = binding
	return decision
}

// snFleetDeviceSigner is the process that holds the client key: the DeviceLocal
// here or, on a phone, across the rpc in the tunnel extension. It repeats the
// client id and key checks itself.
type snFleetDeviceSigner interface {
	GetSnClientKey() string
	SignSnFleetBinding(bindingJson string) (string, error)
	SignSnFleetRevoke(revokeJson string) (string, error)
}

// SnFleetDevice runs a device's half of the claim. Safe for concurrent use.
type SnFleetDevice struct {
	server   snFleetServer
	signer   snFleetDeviceSigner
	clientId connect.Id
	store    SnFleetDeviceStore
	now      func() time.Time
	// [0, n)
	random func(n int64) int64
	sleep  func(ctx context.Context, delay time.Duration) bool

	stateLock sync.Mutex
	cancel    context.CancelFunc
}

func newSnFleetDevice(server snFleetServer, signer snFleetDeviceSigner, clientId connect.Id, store SnFleetDeviceStore) *SnFleetDevice {
	return &SnFleetDevice{
		server:   server,
		signer:   signer,
		clientId: clientId,
		store:    store,
		now:      time.Now,
		random:   rand.Int64N,
		sleep: func(ctx context.Context, delay time.Duration) bool {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(delay):
				return true
			}
		},
	}
}

// NewSnFleetDeviceForLocal is a device whose client key is in this process.
func NewSnFleetDeviceForLocal(api *Api, device *DeviceLocal, store SnFleetDeviceStore) *SnFleetDevice {
	return newSnFleetDevice(api, device, device.clientId, store)
}

// NewSnFleetDeviceForRemote is a device whose client key is in the tunnel
// extension. The checks run here, and the extension signs.
func NewSnFleetDeviceForRemote(api *Api, device *DeviceRemote, store SnFleetDeviceStore) *SnFleetDevice {
	return newSnFleetDevice(api, device, device.clientId, store)
}

// A client key held directly: `provider`, whose key is .provider.key. Slots on one
// host share a key under separate client ids, and each slot has its own.
type snFleetSeedSigner struct {
	snDevice *snDevice
}

func (self *snFleetSeedSigner) GetSnClientKey() string {
	return self.snDevice.GetSnClientKey()
}

func (self *snFleetSeedSigner) SignSnFleetBinding(bindingJson string) (string, error) {
	return self.snDevice.SignSnFleetBinding(bindingJson)
}

func (self *snFleetSeedSigner) SignSnFleetRevoke(revokeJson string) (string, error) {
	return self.snDevice.SignSnFleetRevoke(revokeJson)
}

// NewSnFleetDeviceWithClientKey is for `provider`, which holds its client key
// itself. The context says it isn't for bindings: the key doesn't cross to an app.
func NewSnFleetDeviceWithClientKey(ctx context.Context, api *Api, clientId connect.Id, clientKeySeed []byte, store SnFleetDeviceStore) *SnFleetDevice {
	seed := append([]byte(nil), clientKeySeed...)
	return newSnFleetDevice(api, &snFleetSeedSigner{snDevice: &snDevice{ctx: ctx, clientId: clientId, clientKeySeed: func() []byte { return seed }}}, clientId, store)
}

func (self *SnFleetDevice) networkId() (connect.Id, error) {
	networkId, ok := credentialNetworkId(self.server.snFleetByJwt())
	if !ok {
		return connect.Id{}, errors.New("this device isn't signed in")
	}
	return networkId, nil
}

func (self *SnFleetDevice) clientKey() ([]byte, error) {
	key := self.signer.GetSnClientKey()
	if key == "" {
		return nil, errors.New("the client key isn't available right now")
	}
	return snFleetParseHex(key)
}

// The next check's delay: the server's choice, inside the bounds, give or take a
// quarter.
func (self *SnFleetDevice) nextCheck(serverSeconds int64) time.Duration {
	delay := snFleetDeviceCheckDefault
	if 0 < serverSeconds {
		delay = max(snFleetDeviceCheckMin, min(snFleetDeviceCheckMax, time.Duration(serverSeconds)*time.Second))
	}
	spread := int64(delay / 2)
	return delay - delay/4 + time.Duration(self.random(spread))
}

func (self *SnFleetDevice) saveState(state *snFleetDeviceState) bool {
	encoded, err := json.Marshal(state)
	if err != nil {
		return false
	}
	return self.store.SaveSnFleetDeviceState(string(encoded))
}

// CheckSync asks the server once and does what it asks, if the checks allow it.
//
//gomobile:noexport
func (self *SnFleetDevice) CheckSync(ctx context.Context) (*SnFleetDeviceCheckResult, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.checkWithLock(ctx)
}

func (self *SnFleetDevice) checkWithLock(ctx context.Context) (*SnFleetDeviceCheckResult, error) {
	networkId, err := self.networkId()
	if err != nil {
		return nil, err
	}
	state, err := snFleetLoadDeviceState(self.store.LoadSnFleetDeviceState(), networkId, self.clientId)
	if err != nil {
		// no request either: nothing it could answer is usable
		result := snFleetDeviceRefusal(SnFleetRefusalStateUnreadable, "%s, so this device stopped signing.", err)
		result.NextCheckSeconds = int64(self.nextCheck(0) / time.Second)
		return result, nil
	}
	work, err := self.server.snFleetDeviceWork(ctx, state.Coldkey == "")
	if err != nil {
		return nil, err
	}
	if work == nil {
		return nil, errSnFleetNoResult
	}
	result := &SnFleetDeviceCheckResult{Action: SnFleetDeviceActionNone}
	if err := self.applyWork(ctx, state, networkId, work, result); err != nil {
		return nil, err
	}
	result.BindingOpen = work.BindingOpen
	result.NextCheckSeconds = int64(self.nextCheck(work.NextCheckSeconds) / time.Second)
	return result, nil
}

func (self *SnFleetDevice) applyWork(ctx context.Context, state *snFleetDeviceState, networkId connect.Id, work *snFleetDeviceWork, result *SnFleetDeviceCheckResult) error {
	stateChanged := false
	if work.Consent != nil {
		refusal, changed := snFleetPinConsent(state, networkId, work.Consent)
		if refusal != nil {
			*result = *refusal
			if work.BindingJson == "" {
				return nil
			}
		}
		if changed {
			stateChanged = true
			result.Action = SnFleetDeviceActionPinned
		}
	}
	if work.BindingJson == "" {
		if stateChanged && !self.saveState(state) {
			return errors.New("the fleet state could not be saved")
		}
		return nil
	}
	clientKey, err := self.clientKey()
	if err != nil {
		return err
	}
	decision := snFleetCheckBinding(state, networkId, self.clientId, clientKey, work)
	if decision.refusal != nil {
		if stateChanged && !self.saveState(state) {
			return errors.New("the fleet state could not be saved")
		}
		*result = *decision.refusal
		// the server stops offering it. A device that can't say so still refused
		if _, err := self.server.snFleetDeviceSignature(ctx, &snFleetDeviceSignatureArgs{Digest: decision.digest, Refusal: decision.refusal.RefusalCode}); err != nil {
			result.SendFailed = true
		}
		return nil
	}
	signed := decision.signed
	action := SnFleetDeviceActionResent
	if signed == nil {
		signature, err := self.signer.SignSnFleetBinding(work.BindingJson)
		if err != nil {
			return err
		}
		signed = &snFleetSignedBinding{
			Digest:          decision.digest,
			Generation:      int64(decision.binding.Generation),
			BindingJson:     work.BindingJson,
			HotkeySignature: work.HotkeySignature,
			ClientSignature: signature,
			Authorization:   work.Authorization,
			SignedMillis:    self.now().UnixMilli(),
		}
		state.Signed = append(state.Signed, signed)
		if snFleetDeviceMaxSigned < len(state.Signed) {
			state.Signed = state.Signed[len(state.Signed)-snFleetDeviceMaxSigned:]
		}
		state.Hotkey = snFleetHex(decision.binding.Hotkey[:])
		stateChanged = true
		action = SnFleetDeviceActionSigned
	}
	// saved before it leaves: a restart finds it and doesn't sign again
	if stateChanged && !self.saveState(state) {
		return errors.New("the fleet state could not be saved, so the signature was not sent")
	}
	*result = SnFleetDeviceCheckResult{Action: action, Generation: signed.Generation}
	sent, err := self.server.snFleetDeviceSignature(ctx, &snFleetDeviceSignatureArgs{Digest: signed.Digest, ClientSignature: signed.ClientSignature})
	if err != nil || sent == nil || !sent.Accepted {
		result.SendFailed = true
	}
	return nil
}

// Check is CheckSync for the apps.
func (self *SnFleetDevice) Check(callback SnFleetDeviceCheckCallback) {
	go connect.HandleError(func() {
		ctx, cancel := context.WithTimeout(context.Background(), snFleetDeviceCheckTimeout)
		defer cancel()
		result, err := self.CheckSync(ctx)
		if callback != nil {
			callback.Result(result, err)
		}
	})
}

// RunWithContext checks until the context ends: the first check at a random point
// within the slow interval, so a fleet that restarts together doesn't ask together,
// then when the server said, jittered. A failed check waits the whole slow interval:
// this never retries faster because something failed.
//
//gomobile:noexport
func (self *SnFleetDevice) RunWithContext(ctx context.Context, listener SnFleetDeviceListener) {
	delay := self.firstCheck()
	for {
		if !self.sleep(ctx, delay) {
			return
		}
		checkCtx, cancel := context.WithTimeout(ctx, snFleetDeviceCheckTimeout)
		result, err := self.CheckSync(checkCtx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		var status *connect.HttpStatusError
		if errors.As(err, &status) && status.StatusCode == http.StatusNotFound {
			delay = self.nextCheck(int64(snFleetDeviceCheckNoRoute / time.Second))
		} else if err != nil {
			delay = self.nextCheck(0)
		} else {
			delay = time.Duration(result.NextCheckSeconds) * time.Second
			if listener != nil {
				listener.SnFleetDeviceChecked(result)
			}
		}
		self.saveNextCheck(delay)
	}
}

// When a run's first check is due: when an earlier run's server said, so a restart
// doesn't ask sooner, or a random point within the slow interval, so a fleet that
// restarts together doesn't ask together. An overdue check comes within the hour,
// spread out. A schedule further off than any answer gives is a clock that moved.
func (self *SnFleetDevice) firstCheck() time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if networkId, err := self.networkId(); err == nil {
		state, err := snFleetLoadDeviceState(self.store.LoadSnFleetDeviceState(), networkId, self.clientId)
		if err == nil && 0 < state.NextCheckMillis {
			due := time.UnixMilli(state.NextCheckMillis).Sub(self.now())
			switch {
			case due <= 0:
				return time.Duration(self.random(int64(snFleetDeviceCheckOverdue)))
			case due <= snFleetDeviceCheckMax+snFleetDeviceCheckMax/4:
				return due
			}
		}
	}
	return time.Duration(self.random(int64(snFleetDeviceCheckDefault)))
}

// Saves when the next check is due. A state that can't be read isn't replaced, and
// a save that fails only means the next run picks its own time.
func (self *SnFleetDevice) saveNextCheck(delay time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	networkId, err := self.networkId()
	if err != nil {
		return
	}
	state, err := snFleetLoadDeviceState(self.store.LoadSnFleetDeviceState(), networkId, self.clientId)
	if err != nil {
		return
	}
	state.NextCheckMillis = self.now().Add(delay).UnixMilli()
	self.saveState(state)
}

// Start runs the checks in the background until Close.
func (self *SnFleetDevice) Start(listener SnFleetDeviceListener) {
	ctx, cancel := context.WithCancel(context.Background())
	self.stateLock.Lock()
	if self.cancel != nil {
		self.cancel()
	}
	self.cancel = cancel
	self.stateLock.Unlock()
	go connect.HandleError(func() {
		self.RunWithContext(ctx, listener)
	})
}

// Close stops what Start started.
func (self *SnFleetDevice) Close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.cancel != nil {
		self.cancel()
		self.cancel = nil
	}
}

// LeaveSync takes this device out of its fleet from the next epoch. It needs only
// the client key, and the server's relay pays. It first saves that this device
// left, so nothing more gets signed even if the rest fails, and running it again
// finishes the job.
//
//gomobile:noexport
func (self *SnFleetDevice) LeaveSync(ctx context.Context) (*SnFleetDeviceLeaveOutcome, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	networkId, err := self.networkId()
	if err != nil {
		return nil, err
	}
	state, err := snFleetLoadDeviceState(self.store.LoadSnFleetDeviceState(), networkId, self.clientId)
	if err != nil {
		return nil, err
	}
	if !state.Left {
		state.Left = true
		if !self.saveState(state) {
			return nil, errors.New("the fleet state could not be saved")
		}
	}
	// one retry: the epoch can turn over between the read and the request
	for range 2 {
		binding, err := self.server.snFleetDeviceBinding(ctx)
		if err != nil {
			return nil, err
		}
		if binding == nil {
			return nil, errSnFleetNoResult
		}
		args := &snFleetDeviceLeaveArgs{}
		effective := int64(0)
		if binding.Bound && binding.CurrentEpoch < binding.ValidToEpoch {
			effective = binding.CurrentEpoch + 1
			signature, err := self.signRevoke(state, binding, effective)
			if err != nil {
				return nil, err
			}
			args = &snFleetDeviceLeaveArgs{Generation: binding.Generation, EffectiveEpoch: effective, ClientSignature: signature}
		}
		result, err := self.server.snFleetDeviceLeave(ctx, args)
		if err != nil {
			return nil, err
		}
		if result == nil {
			return nil, errSnFleetNoResult
		}
		if result.Error != nil {
			return nil, errors.New(result.Error.Message)
		}
		if result.Left {
			// Leave signs with the client key, not a key of the user's funds
			return &SnFleetDeviceLeaveOutcome{Left: true, EffectiveEpoch: result.EffectiveEpoch, FundsDisclaimer: snFleetFundsDisclaimer(false)}, nil
		}
		if result.CurrentEpoch == binding.CurrentEpoch {
			break
		}
	}
	return nil, errors.New("the server didn't take the leave; run it again")
}

// The revocation to sign. A deployment this device saved from its own binding is
// used over the one the server names, and if they differ the device says so.
func (self *SnFleetDevice) signRevoke(state *snFleetDeviceState, binding *snFleetDeviceBinding, effective int64) (string, error) {
	coordinator := binding.Coordinator
	chainId, netuid := binding.ChainId, binding.Netuid
	for _, signed := range state.Signed {
		if signed.Generation != binding.Generation {
			continue
		}
		saved, err := snParseFleetBinding(signed.BindingJson)
		if err != nil {
			continue
		}
		if saved.ChainID != uint64(chainId) || int64(saved.Netuid) != netuid || snFleetHex(saved.Coordinator[:]) != strings.ToLower(coordinator) {
			return "", errors.New("the server names another deployment than this device's binding")
		}
	}
	revoke, err := json.Marshal(map[string]any{
		"chain_id":        chainId,
		"netuid":          netuid,
		"coordinator":     coordinator,
		"client_id":       self.clientId.String(),
		"generation":      binding.Generation,
		"effective_epoch": effective,
	})
	if err != nil {
		return "", err
	}
	return self.signer.SignSnFleetRevoke(string(revoke))
}

// Leave is LeaveSync for the apps.
func (self *SnFleetDevice) Leave(callback SnFleetDeviceLeaveCallback) {
	go connect.HandleError(func() {
		ctx, cancel := context.WithTimeout(context.Background(), snFleetDeviceCheckTimeout)
		defer cancel()
		result, err := self.LeaveSync(ctx)
		if callback != nil {
			callback.Result(result, err)
		}
	})
}

// Reset forgets what this device pinned and that it left, after a person decided a
// changed coldkey or hotkey is real or wants back in. The bindings it signed stay,
// so it still won't sign two for one generation. The next consent is a first sight.
func (self *SnFleetDevice) Reset() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	networkId, err := self.networkId()
	if err != nil {
		return err
	}
	state, err := snFleetLoadDeviceState(self.store.LoadSnFleetDeviceState(), networkId, self.clientId)
	if err != nil {
		// nothing readable to keep
		state = &snFleetDeviceState{Schema: snFleetDeviceStateSchema, NetworkId: networkId.String(), ClientId: self.clientId.String()}
	}
	state.Coldkey, state.ConsentGeneration, state.Hotkey, state.Left = "", 0, "", false
	if !self.saveState(state) {
		return errors.New("the fleet state could not be saved")
	}
	return nil
}
