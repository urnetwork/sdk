package subtensor

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
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

// The keys of a hotkey's owner and of the subnet taking registrations, as read on
// mainnet on 2026-11-03 at spec 486: UID 70's hotkey and SN25.
func TestReadRegistrationChecksFromMainnetBytes(t *testing.T) {
	hotkey := fleetReadsHotkey(t, "9834e4fc47eb07e0b61b0968d03ddcd8b4ac9e7d374adfa853ad1c4ad8871061")
	if key := ownerStorageKey(hotkey); key != "0x658faa385070e074c85bf6b568cf0555eca6b7a1fdc9f689184ecb4f359c0518b8bbc92996f5c1e5acfbdc5791f9b26e9834e4fc47eb07e0b61b0968d03ddcd8b4ac9e7d374adfa853ad1c4ad8871061" {
		t.Fatalf("owner key %s", key)
	}
	if key := registrationAllowedStorageKey(25); key != "0x658faa385070e074c85bf6b568cf0555d5fe74da02c7b4bbb340fb368eee3e771900" {
		t.Fatalf("allowed key %s", key)
	}
	coldkey := fleetReadsHotkey(t, "94732473f442f5aa2d9ed55054f4548125da672c7c9a1acaf43097aa25493166")
	chain := NewChain(&fleetReadsRpc{storage: map[string]string{
		ownerStorageKey(hotkey):           "0x" + hex.EncodeToString(coldkey[:]),
		registrationAllowedStorageKey(25): "0x01",
	}})
	owner, found, err := chain.ReadHotkeyOwner(context.Background(), hotkey)
	if err != nil || !found || owner != coldkey {
		t.Fatalf("owner %x %t %v", owner, found, err)
	}
	if allowed, err := chain.ReadRegistrationAllowed(context.Background(), 25); err != nil || !allowed {
		t.Fatalf("%t %v", allowed, err)
	}

	empty := NewChain(&fleetReadsRpc{storage: map[string]string{
		ownerStorageKey(hotkey):           "0x" + strings.Repeat("00", 32),
		registrationAllowedStorageKey(26): "0x00",
	}})
	if _, found, err := empty.ReadHotkeyOwner(context.Background(), hotkey); err != nil || found {
		t.Fatalf("the zero account is no owner: %t %v", found, err)
	}
	if _, found, err := empty.ReadHotkeyOwner(context.Background(), [32]byte{9}); err != nil || found {
		t.Fatalf("an absent owner: %t %v", found, err)
	}
	if allowed, err := empty.ReadRegistrationAllowed(context.Background(), 25); err != nil || !allowed {
		t.Fatalf("absent is allowed: %t %v", allowed, err)
	}
	if allowed, err := empty.ReadRegistrationAllowed(context.Background(), 26); err != nil || allowed {
		t.Fatalf("closed: %t %v", allowed, err)
	}
	odd := NewChain(&fleetReadsRpc{storage: map[string]string{registrationAllowedStorageKey(25): "0x02", ownerStorageKey(hotkey): "0x01"}})
	if _, err := odd.ReadRegistrationAllowed(context.Background(), 25); err == nil {
		t.Fatal("an odd bool read")
	}
	if _, _, err := odd.ReadHotkeyOwner(context.Background(), hotkey); err == nil {
		t.Fatal("a one-byte owner read")
	}
	failed := NewChain(&fleetReadsRpc{err: errors.New("down")})
	if allowed, err := failed.ReadRegistrationAllowed(context.Background(), 25); err == nil || allowed {
		t.Fatal("a failed read looked allowed")
	}
}
