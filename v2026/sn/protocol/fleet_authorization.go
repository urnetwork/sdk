package protocol

// The coldkey's authorization of a fleet hotkey.
//
// The dual signature on a binding exists so a fleet can't claim a client it
// doesn't operate. A device that signs by itself needs its own reason to trust
// the hotkey it is asked to join: a signature from the network's coldkey over
// this message, naming the network, the chain's coordinates and the hotkey.
//
// This is the one copy of the format. The server, the apps and `provider` all
// use it. The signature checks are in fleet_signature.go.

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/urnetwork/sdk/v2026/sn/ss58"
)

const (
	// FleetHotkeyAuthorizationPrefix is the first line of every authorization.
	// No network wallet consent, provider consent or sign-in message starts
	// with it, and each of those decoders requires its own first line, so a
	// signature over one kind never verifies as another.
	FleetHotkeyAuthorizationPrefix = "Authorize URnetwork fleet hotkey\n"
	FleetHotkeyAuthorizationSchema = "urnetwork-fleet-hotkey-authorization-v1"

	// What a person reads in the wallet that shows this message.
	fleetHotkeyAuthorizationNotice = "This lets the hotkey below claim a Top 200 spot for this network's devices. It can't move funds."

	// the Bittensor ss58 network prefix
	fleetHotkeyAuthorizationSs58Prefix = 42

	// a message longer than this is not one of ours
	fleetHotkeyAuthorizationMaxBytes = 1024
)

// FleetHotkeyAuthorization is what the coldkey approves: this hotkey may act
// for this network's fleet on this deployment.
type FleetHotkeyAuthorization struct {
	ChainID     uint64
	Netuid      uint16
	Coordinator [20]byte
	NetworkID   [16]byte
	Coldkey     [32]byte
	Hotkey      [32]byte
}

// Validate rejects zero identities, and a hotkey equal to the coldkey: the plan
// keeps them separate so a device never holds or names the wallet's key as a
// fleet's.
func (self FleetHotkeyAuthorization) Validate() error {
	if self.ChainID == 0 || self.Netuid == 0 {
		return errors.New("fleet hotkey authorization chain_id/netuid is zero")
	}
	if self.Coordinator == ([20]byte{}) || self.NetworkID == ([16]byte{}) || self.Coldkey == ([32]byte{}) || self.Hotkey == ([32]byte{}) {
		return errors.New("fleet hotkey authorization contains a zero identity")
	}
	if self.Coldkey == self.Hotkey {
		return errors.New("fleet hotkey authorization names the coldkey as its hotkey")
	}
	return nil
}

func fleetAuthorizationUuid(id [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
}

// Message is the exact text the coldkey signs: fixed lines in a fixed order,
// with no trailing newline, so one authorization has exactly one encoding.
func (self FleetHotkeyAuthorization) Message() (string, error) {
	if err := self.Validate(); err != nil {
		return "", err
	}
	coldkey, err := ss58.Encode(self.Coldkey, fleetHotkeyAuthorizationSs58Prefix)
	if err != nil {
		return "", err
	}
	hotkey, err := ss58.Encode(self.Hotkey, fleetHotkeyAuthorizationSs58Prefix)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	out.WriteString(FleetHotkeyAuthorizationPrefix)
	out.WriteString(fleetHotkeyAuthorizationNotice + "\n")
	fmt.Fprintf(&out, "schema: %s\n", FleetHotkeyAuthorizationSchema)
	fmt.Fprintf(&out, "chain_id: %d\n", self.ChainID)
	fmt.Fprintf(&out, "netuid: %d\n", self.Netuid)
	fmt.Fprintf(&out, "coordinator: 0x%s\n", hex.EncodeToString(self.Coordinator[:]))
	fmt.Fprintf(&out, "network_id: %s\n", fleetAuthorizationUuid(self.NetworkID))
	fmt.Fprintf(&out, "coldkey: %s\n", coldkey)
	fmt.Fprintf(&out, "hotkey: %s", hotkey)
	return out.String(), nil
}

// ParseFleetHotkeyAuthorization reads a message and accepts it only when it is
// exactly what Message produces for the values it names. A consent or any other
// signed text fails here, whatever its signature.
func ParseFleetHotkeyAuthorization(message string) (*FleetHotkeyAuthorization, error) {
	if fleetHotkeyAuthorizationMaxBytes < len(message) {
		return nil, errors.New("fleet hotkey authorization is too long")
	}
	rest, ok := strings.CutPrefix(message, FleetHotkeyAuthorizationPrefix+fleetHotkeyAuthorizationNotice+"\n")
	if !ok {
		return nil, errors.New("message is not a fleet hotkey authorization")
	}
	values := map[string]string{}
	lines := strings.Split(rest, "\n")
	for _, line := range lines {
		key, value, found := strings.Cut(line, ": ")
		if !found {
			return nil, errors.New("fleet hotkey authorization has a malformed line")
		}
		if _, seen := values[key]; seen {
			return nil, fmt.Errorf("fleet hotkey authorization repeats %q", key)
		}
		values[key] = value
	}
	if values["schema"] != FleetHotkeyAuthorizationSchema {
		return nil, errors.New("fleet hotkey authorization has another schema")
	}
	out := &FleetHotkeyAuthorization{}
	chainId, err := strconv.ParseUint(values["chain_id"], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("fleet hotkey authorization chain_id: %w", err)
	}
	out.ChainID = chainId
	netuid, err := strconv.ParseUint(values["netuid"], 10, 16)
	if err != nil {
		return nil, fmt.Errorf("fleet hotkey authorization netuid: %w", err)
	}
	out.Netuid = uint16(netuid)
	coordinator, err := hex.DecodeString(strings.TrimPrefix(values["coordinator"], "0x"))
	if err != nil || len(coordinator) != len(out.Coordinator) {
		return nil, errors.New("fleet hotkey authorization coordinator must be a 20-byte address")
	}
	copy(out.Coordinator[:], coordinator)
	networkId, err := hex.DecodeString(strings.ReplaceAll(values["network_id"], "-", ""))
	if err != nil || len(networkId) != len(out.NetworkID) {
		return nil, errors.New("fleet hotkey authorization network_id must be a uuid")
	}
	copy(out.NetworkID[:], networkId)
	if out.Coldkey, err = ss58.DecodeWithPrefix(values["coldkey"], fleetHotkeyAuthorizationSs58Prefix); err != nil {
		return nil, fmt.Errorf("fleet hotkey authorization coldkey: %w", err)
	}
	if out.Hotkey, err = ss58.DecodeWithPrefix(values["hotkey"], fleetHotkeyAuthorizationSs58Prefix); err != nil {
		return nil, fmt.Errorf("fleet hotkey authorization hotkey: %w", err)
	}
	canonical, err := out.Message()
	if err != nil {
		return nil, err
	}
	if canonical != message {
		return nil, errors.New("fleet hotkey authorization is not canonical")
	}
	return out, nil
}
