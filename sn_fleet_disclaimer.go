//go:build !ios_extension && !js

package sdk

// The funds disclaimer of the easy flows (owner, 2026-11-03). The Top 200 claim, its
// progress, its finished state and Leave are easy flows, so every SnFleetClaimState and
// SnFleetDeviceLeaveOutcome carries it, and every app shows it on each Top 200 screen:
// the app's own text for FundsDisclaimerKey, or FundsDisclaimer.
//
// The whole text when a key of the claim is on this device, which on the existing
// wallet path too is the fleet hotkey this device made. Only its last sentence when
// no such key is: the hotkey is on another device, nothing is made yet, or Leave,
// which signs with the device's client key.
//
// The seam: the Bittensor flows own the string key and are adding it, with a flow
// field (feat/bittensor-funds-disclaimer). Until that lands these are the claim's own.
// Then snFleetFundsDisclaimer returns that key, and nothing else here changes.
const (
	SnFleetFundsDisclaimerKeyFull  = "sn_fleet_funds_disclaimer_full"
	SnFleetFundsDisclaimerKeyFunds = "sn_fleet_funds_disclaimer_funds"

	SnFleetFundsDisclaimerTextFull  = "This key is stored locally in the app secure storage. It is never shared with URnetwork. " + SnFleetFundsDisclaimerTextFunds
	SnFleetFundsDisclaimerTextFunds = "URnetwork does not touch and is not responsible for any user funds."
)

// The one place a Top 200 screen's disclaimer is chosen.
func snFleetFundsDisclaimer(keyOnDevice bool) (key string, text string) {
	if keyOnDevice {
		return SnFleetFundsDisclaimerKeyFull, SnFleetFundsDisclaimerTextFull
	}
	return SnFleetFundsDisclaimerKeyFunds, SnFleetFundsDisclaimerTextFunds
}
