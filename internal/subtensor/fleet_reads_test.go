package subtensor

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/urnetwork/sdk/sn/evm"
)

// Answers state_getStorage from a table, as a node would.
type fleetReadsRpc struct {
	storage map[string]string
	err     error
}

func (self *fleetReadsRpc) Call(_ context.Context, method string, params ...any) (json.RawMessage, error) {
	if self.err != nil {
		return nil, self.err
	}
	if method != "state_getStorage" || len(params) != 1 {
		return nil, errors.New("unexpected call " + method)
	}
	value, ok := self.storage[params[0].(string)]
	if !ok {
		return json.RawMessage("null"), nil
	}
	return json.Marshal(value)
}

func (self *fleetReadsRpc) Batch(context.Context, []evm.BatchCall) ([]evm.BatchResult, error) {
	return nil, errors.New("unexpected batch")
}

func fleetReadsHotkey(t *testing.T, value string) [32]byte {
	t.Helper()
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != 32 {
		t.Fatal(value)
	}
	return [32]byte(raw)
}

// Keys and values read from mainnet at block 9,422,190 for UID 70's hotkey, whose
// commitment is in the tagged fleet format.
func TestReadFleetValuesFromMainnetBytes(t *testing.T) {
	hotkey := fleetReadsHotkey(t, "9834e4fc47eb07e0b61b0968d03ddcd8b4ac9e7d374adfa853ad1c4ad8871061")
	wantHash := fleetReadsHotkey(t, "74c3e9121899dd24093d90a511f0c56b8cbc21c297cadfe455e0ede7992e6e37")
	rpc := &fleetReadsRpc{storage: map[string]string{
		uidsStorageKey(25, hotkey):         "0x4600",
		commitmentOfStorageKey(25, hotkey): "0x0000000000000000286c8e0004297572666c6565743174c3e9121899dd24093d90a511f0c56b8cbc21c297cadfe455e0ede7992e6e37",
	}}
	chain := NewChain(rpc)
	uid, registered, err := chain.ReadHotkeyUid(context.Background(), 25, hotkey)
	if err != nil || !registered || uid != 70 {
		t.Fatalf("uid %d %t %v", uid, registered, err)
	}
	hash, found, err := chain.ReadFleetCommitment(context.Background(), 25, hotkey)
	if err != nil || !found || hash != wantHash {
		t.Fatalf("hash %x %t %v", hash, found, err)
	}
	// what was read on mainnet went to these exact keys
	if key := uidsStorageKey(25, hotkey); key[:66] != "0x"+storageSubtensorUids[:64] {
		t.Fatalf("uids key %s", key)
	}
}

func TestReadFleetValuesAbsentAndOtherFormats(t *testing.T) {
	hotkey := [32]byte{1}
	chain := NewChain(&fleetReadsRpc{storage: map[string]string{}})
	if _, registered, err := chain.ReadHotkeyUid(context.Background(), 25, hotkey); err != nil || registered {
		t.Fatalf("%t %v", registered, err)
	}
	if _, found, err := chain.ReadFleetCommitment(context.Background(), 25, hotkey); err != nil || found {
		t.Fatalf("%t %v", found, err)
	}
	for name, value := range map[string]string{
		// the old one-field Sha256 form: not a fleet commitment
		"legacy":      "0x0000000000000000286c8e0004" + "85" + "74c3e9121899dd24093d90a511f0c56b8cbc21c297cadfe455e0ede7992e6e37",
		"another tag": "0x0000000000000000286c8e000429" + "7572666c65657432" + "74c3e9121899dd24093d90a511f0c56b8cbc21c297cadfe455e0ede7992e6e37",
		"two fields":  "0x0000000000000000286c8e000829" + "7572666c65657431" + "74c3e9121899dd24093d90a511f0c56b8cbc21c297cadfe455e0ede7992e6e37",
		"zero hash":   "0x0000000000000000286c8e000429" + "7572666c65657431" + "0000000000000000000000000000000000000000000000000000000000000000",
	} {
		chain := NewChain(&fleetReadsRpc{storage: map[string]string{commitmentOfStorageKey(25, hotkey): value}})
		if _, found, err := chain.ReadFleetCommitment(context.Background(), 25, hotkey); err != nil || found {
			t.Errorf("%s: %t %v", name, found, err)
		}
	}
	short := NewChain(&fleetReadsRpc{storage: map[string]string{uidsStorageKey(25, hotkey): "0x46"}})
	if _, _, err := short.ReadHotkeyUid(context.Background(), 25, hotkey); err == nil {
		t.Fatal("a one-byte uid read")
	}
	failed := NewChain(&fleetReadsRpc{err: errors.New("down")})
	if _, registered, err := failed.ReadHotkeyUid(context.Background(), 25, hotkey); err == nil || registered {
		t.Fatal("a failed read looked unregistered")
	}
}
