package subtensor

// The two reads a Top 200 claim needs beyond what a convert reads: whether a hotkey
// holds a UID, and what it last published. Both go by exact bytes, as the coordinator's
// oracle does (sn commitmentoracle, crv4), so anything that isn't the tagged fleet
// format reads as not published.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"

	"github.com/cespare/xxhash/v2"
	"golang.org/x/crypto/blake2b"
)

const (
	// twox128("SubtensorModule") ‖ twox128("Uids"), then the netuid (identity) and
	// the hotkey (blake2_128 concat). Checked against mainnet's UIDs 70, 194 and 255.
	storageSubtensorUids = "658faa385070e074c85bf6b568cf0555aab1b4e78e1ea8305462ee53b3686dc8"
	// twox128("Commitments") ‖ twox128("CommitmentOf"), then the netuid (identity)
	// and the hotkey (twox64 concat). Checked against the same three.
	storageCommitmentOf = "ca407206ec1ab726b2636c4b145ac287419a60ae8b01e6dcaebd7317e43c69bf"

	// twox128("SubtensorModule") ‖ twox128("Owner"), then the hotkey (blake2_128
	// concat): the coldkey a hotkey belongs to. Checked against mainnet's UID 70.
	storageSubtensorOwner = "658faa385070e074c85bf6b568cf0555eca6b7a1fdc9f689184ecb4f359c0518"
	// twox128("SubtensorModule") ‖ twox128("NetworkRegistrationAllowed"), then the
	// netuid (identity). Checked against mainnet's SN25.
	storageNetworkRegistrationAllowed = "658faa385070e074c85bf6b568cf0555d5fe74da02c7b4bbb340fb368eee3e77"

	// a u64 deposit, a u32 block, compact vec length 1, the Raw40 variant, then the
	// tag and the 32-byte hash
	fleetRegistrationBytes = 8 + 4 + 1 + 1 + len(FleetCommitmentTag) + 32
)

func uidsStorageKey(netuid uint16, hotkey [32]byte) string {
	suffix := binary.LittleEndian.AppendUint16(nil, netuid)
	hash, err := blake2b.New(16, nil)
	if err != nil {
		panic(err)
	}
	hash.Write(hotkey[:])
	suffix = hash.Sum(suffix)
	suffix = append(suffix, hotkey[:]...)
	return storageKey(storageSubtensorUids, suffix)
}

func commitmentOfStorageKey(netuid uint16, hotkey [32]byte) string {
	suffix := binary.LittleEndian.AppendUint16(nil, netuid)
	suffix = binary.LittleEndian.AppendUint64(suffix, xxhash.Sum64(hotkey[:]))
	suffix = append(suffix, hotkey[:]...)
	return storageKey(storageCommitmentOf, suffix)
}

func ownerStorageKey(hotkey [32]byte) string {
	hash, err := blake2b.New(16, nil)
	if err != nil {
		panic(err)
	}
	hash.Write(hotkey[:])
	suffix := hash.Sum(nil)
	suffix = append(suffix, hotkey[:]...)
	return storageKey(storageSubtensorOwner, suffix)
}

func registrationAllowedStorageKey(netuid uint16) string {
	return storageKey(storageNetworkRegistrationAllowed, binary.LittleEndian.AppendUint16(nil, netuid))
}

// parseOptionalStorage is a state_getStorage answer's bytes, nil for an absent key.
func parseOptionalStorage(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	return parseRawHexBytes(raw)
}

// ReadHotkeyUid is the UID a hotkey holds on the subnet.
func (self *Chain) ReadHotkeyUid(ctx context.Context, netuid uint16, hotkey [32]byte) (uid uint16, registered bool, err error) {
	raw, err := self.rpc.Call(ctx, "state_getStorage", uidsStorageKey(netuid, hotkey))
	if err != nil {
		return 0, false, err
	}
	value, err := parseOptionalStorage(raw)
	if err != nil {
		return 0, false, err
	}
	if value == nil {
		return 0, false, nil
	}
	if len(value) != 2 {
		return 0, false, runtimeChanged("SubtensorModule.Uids is %d bytes", len(value))
	}
	return binary.LittleEndian.Uint16(value), true, nil
}

// ReadFleetCommitment is the hash a hotkey last published in the tagged fleet format.
func (self *Chain) ReadFleetCommitment(ctx context.Context, netuid uint16, hotkey [32]byte) (hash [32]byte, found bool, err error) {
	raw, err := self.rpc.Call(ctx, "state_getStorage", commitmentOfStorageKey(netuid, hotkey))
	if err != nil {
		return hash, false, err
	}
	value, err := parseOptionalStorage(raw)
	if err != nil {
		return hash, false, err
	}
	hash, found = parseFleetRegistration(value)
	return hash, found, nil
}

func parseFleetRegistration(value []byte) (hash [32]byte, found bool) {
	if len(value) != fleetRegistrationBytes || value[12] != 0x04 || value[13] != commitmentDataRaw40 {
		return hash, false
	}
	field := value[14:]
	if string(field[:len(FleetCommitmentTag)]) != FleetCommitmentTag {
		return hash, false
	}
	copy(hash[:], field[len(FleetCommitmentTag):])
	return hash, hash != ([32]byte{})
}

// ReadHotkeyOwner is the coldkey a hotkey belongs to, none for a hotkey no coldkey
// has registered. Registering a hotkey another coldkey owns fails after its fee.
func (self *Chain) ReadHotkeyOwner(ctx context.Context, hotkey [32]byte) (owner [32]byte, found bool, err error) {
	raw, err := self.rpc.Call(ctx, "state_getStorage", ownerStorageKey(hotkey))
	if err != nil {
		return owner, false, err
	}
	value, err := parseOptionalStorage(raw)
	if err != nil {
		return owner, false, err
	}
	if value == nil {
		return owner, false, nil
	}
	if len(value) != 32 {
		return owner, false, runtimeChanged("SubtensorModule.Owner is %d bytes", len(value))
	}
	owner = [32]byte(value)
	// the map's fallback is the zero account
	return owner, owner != ([32]byte{}), nil
}

// ReadRegistrationAllowed says whether the subnet takes registrations, which it does
// unless the key says otherwise.
func (self *Chain) ReadRegistrationAllowed(ctx context.Context, netuid uint16) (bool, error) {
	raw, err := self.rpc.Call(ctx, "state_getStorage", registrationAllowedStorageKey(netuid))
	if err != nil {
		return false, err
	}
	value, err := parseOptionalStorage(raw)
	if err != nil {
		return false, err
	}
	switch {
	case value == nil:
		return true, nil
	case len(value) == 1 && value[0] <= 1:
		return value[0] == 1, nil
	}
	return false, runtimeChanged("SubtensorModule.NetworkRegistrationAllowed is %x", value)
}
