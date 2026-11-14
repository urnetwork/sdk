//go:build !ios_extension && !js

package sdk

import (
	"context"

	"github.com/urnetwork/connect/v2026"
)

// The funds disclaimer of the easy flows (owner, 2026-11-03). The Top 200 claim, its
// progress, its finished state and Leave are easy flows, so every SnFleetClaimState and
// SnFleetDeviceLeaveOutcome carries FundsDisclaimer, a BittensorFundsDisclaimer*: the id
// of the string the screen shows, once. The whole text when a key of the claim is on
// this device: the network's coldkey made here, as
// BittensorLocalKeyFlow.FundsDisclaimerAt(BittensorFundsDisclaimerPlaceTop200) says,
// and also the fleet hotkey, which on the existing-wallet path is this device's. The
// last sentence alone when only an outside wallet's coldkey is involved.

// The one place a Top 200 screen's disclaimer is chosen.
func snFleetFundsDisclaimer(keyOnDevice bool) string {
	if keyOnDevice {
		return BittensorFundsDisclaimerFull
	}
	return BittensorFundsDisclaimerFundsOnly
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
