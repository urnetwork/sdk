//go:build !ios_extension && !js

package sdk

import (
	"context"

	"github.com/urnetwork/connect"
)

// The funds disclaimer of the easy flows (owner, 2026-11-03). The Top 200 claim, its
// progress, its finished state and Leave are easy flows, so every SnFleetClaimState and
// SnFleetDeviceLeaveOutcome carries FundsDisclaimer: the id of the string the screen
// shows. The Bittensor flows own the strings:
//
//   - "bittensor_funds_disclaimer", the owner's three sentences, when a key of the
//     claim is on this device: the fleet hotkey, which on the existing-wallet path too
//     is this device's, or the network's coldkey made here;
//   - "bittensor_funds_disclaimer_funds_only", the last sentence, when only an outside
//     wallet's coldkey is involved.
//
// The seam: those ids are BittensorFundsDisclaimerFull and
// BittensorFundsDisclaimerFundsOnly of feat/bittensor-funds-disclaimer, and the flows
// will choose them with FundsDisclaimerAt(place). Until that's on main these are copies
// of the ids, and snFleetFundsDisclaimer is the one place that changes.
const (
	snFleetFundsDisclaimerFull      = "bittensor_funds_disclaimer"
	snFleetFundsDisclaimerFundsOnly = "bittensor_funds_disclaimer_funds_only"
)

// The one place a Top 200 screen's disclaimer is chosen.
func snFleetFundsDisclaimer(keyOnDevice bool) string {
	if keyOnDevice {
		return snFleetFundsDisclaimerFull
	}
	return snFleetFundsDisclaimerFundsOnly
}

// Every state the flow returns carries the disclaimer for what's on this device. A
// state that stopped before the step looked at the keys, as while binding is closed,
// asks the store.
func (self *SnFleetClaim) disclosed(state *SnFleetClaimState) *SnFleetClaimState {
	if !state.keyKnown {
		state.keyOnDevice = self.keyOnDevice()
	}
	state.FundsDisclaimer = snFleetFundsDisclaimer(state.keyOnDevice)
	return state
}

// Whether a key of the session network's claim is on this device, from the app's
// store alone: the fleet hotkey, or a recovery phrase. A store that can't be read
// holds none.
func (self *SnFleetClaim) keyOnDevice() bool {
	networkId, ok := credentialNetworkId(self.server.snFleetByJwt())
	if !ok {
		return false
	}
	if _, _, found, err := self.loadHotkey(networkId); err == nil && found {
		return true
	}
	phrase := self.store.LoadSnFleetColdkeyPhrase(networkId.String())
	if phrase == "" {
		return false
	}
	_, err := self.coldkey.Address(phrase)
	return err == nil
}

// Leave takes the device out of its fleet, as SnFleetDevice.Leave does, with the
// outcome's disclaimer for the keys on this device. Apps leave through this; a
// device on its own, like a provider, has no key of the claim and leaves directly.
func (self *SnFleetClaim) Leave(device *SnFleetDevice, callback SnFleetDeviceLeaveCallback) {
	go connect.HandleError(func() {
		ctx, cancel := context.WithTimeout(context.Background(), snFleetDeviceCheckTimeout)
		defer cancel()
		outcome, err := self.LeaveSync(ctx, device)
		if callback != nil {
			callback.Result(outcome, err)
		}
	})
}

//gomobile:noexport
func (self *SnFleetClaim) LeaveSync(ctx context.Context, device *SnFleetDevice) (*SnFleetDeviceLeaveOutcome, error) {
	outcome, err := device.LeaveSync(ctx)
	if outcome != nil {
		self.stateLock.Lock()
		outcome.FundsDisclaimer = snFleetFundsDisclaimer(self.keyOnDevice())
		self.stateLock.Unlock()
	}
	return outcome, err
}
