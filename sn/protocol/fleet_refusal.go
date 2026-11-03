package protocol

// Why a device refused a binding it was asked to sign. A device reports the code,
// so the network's card can say what stopped, and the server stops offering that
// binding. These are the only values the server stores.
const (
	// the binding names another client id
	FleetRefusalOtherClient = "other_client"
	// the binding names this client id with another key
	FleetRefusalOtherClientKey = "other_client_key"
	// the binding doesn't parse or validate
	FleetRefusalBadBinding = "bad_binding"
	// the hotkey's signature over the binding doesn't verify
	FleetRefusalBadHotkeySignature = "bad_hotkey_signature"
	// the device has seen no consent of its network, so it has no coldkey to trust
	FleetRefusalColdkeyNotPinned = "coldkey_not_pinned"
	// the network's consent doesn't verify or names another network
	FleetRefusalBadConsent = "bad_consent"
	// the network's coldkey isn't the one this device pinned
	FleetRefusalColdkeyChanged = "coldkey_changed"
	// the hotkey isn't the one this device pinned
	FleetRefusalHotkeyChanged = "hotkey_changed"
	// the authorization is missing, doesn't verify or names another network
	FleetRefusalBadAuthorization = "bad_authorization"
	// the binding's hotkey or deployment isn't the one the coldkey authorized
	FleetRefusalNotAuthorized = "hotkey_not_authorized"
	// a different binding for a generation this device has already signed
	FleetRefusalGenerationReplayed = "generation_replayed"
	// this device left the fleet
	FleetRefusalLeft = "device_left"
	// the device's saved state can't be read, so it can't tell what it pinned
	FleetRefusalStateUnreadable = "state_unreadable"
)

// FleetRefusalKnown reports whether a device could have sent this code.
func FleetRefusalKnown(code string) bool {
	switch code {
	case FleetRefusalOtherClient, FleetRefusalOtherClientKey, FleetRefusalBadBinding,
		FleetRefusalBadHotkeySignature, FleetRefusalColdkeyNotPinned, FleetRefusalBadConsent,
		FleetRefusalColdkeyChanged, FleetRefusalHotkeyChanged, FleetRefusalBadAuthorization,
		FleetRefusalNotAuthorized, FleetRefusalGenerationReplayed, FleetRefusalLeft,
		FleetRefusalStateUnreadable:
		return true
	}
	return false
}

// What a one-tap fleet is held to, the same on the server and on the key holder's
// device.
const (
	// The most devices a one-tap fleet binds, each its own relayed transaction: the
	// per-fleet priority share of the verify cohort at a cohort of 1,500, and the
	// relay's limit (BTEASY.md question 15).
	FleetRelayDeviceLimit = 7
	// The longest binding the coordinator accepts on mainnet.
	FleetBindingMaxEpochs = uint64(12)
	// A registration burn above this waits, and a top-up never pays one: 10 times the
	// floor of 0.0005 TAO. It halves about every 72 minutes, and a spike can reach
	// many TAO.
	FleetClaimMaxBurnRao int64 = 5_000_000
)
