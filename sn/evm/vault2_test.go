package evm

// The STSettlementVault2 encodings a v2 claim uses. The goldens are the
// selectors of the vault's ABI and calldata from Foundry's cast, for
// sn/evm/abi/STSettlementVault2.abi.json and the staking precompile's
// IStaking.transferStake.

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/urnetwork/sdk/sn/merkle"
)

func mustHexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := ParseHexBytes(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func word(v uint64) []byte {
	w := uint64Word(v)
	return w[:]
}

// APPS-SPEC claim rules 1, 2 and 5: the selectors are vault2's (cast sig) and
// the event topic its Claimed (cast keccak).
func TestLedgerSelectors(t *testing.T) {
	for name, c := range map[string]struct {
		got  [4]byte
		want string
	}{
		"claim":         {ledgerClaimSelector, "b7a6b907"},
		"roots":         {ledgerRootsSelector, "0fc3cec0"},
		"claimed":       {ledgerClaimedSelector, "a54ab457"},
		"escrowHotkey":  {escrowHotkeySelector, "164bf360"},
		"transferStake": {transferStakeSelector, "17ce5f62"},
	} {
		if got := hex.EncodeToString(c.got[:]); got != c.want {
			t.Fatalf("%s selector %s, want %s", name, got, c.want)
		}
	}
	if got := hex.EncodeToString(LedgerClaimedTopic[:]); got != "ea86c1b3284a090510a2098141252236197ab9664d20cf7a6d43ea767453bc2b" {
		t.Fatalf("Claimed topic %s", got)
	}
	if AddressHex(StakingPrecompile) != "0x0000000000000000000000000000000000000805" {
		t.Fatalf("staking precompile %s", AddressHex(StakingPrecompile))
	}
}

// APPS-SPEC claim rule 2: claim(noId, rootIndex, (index, coldkey, cumulative), proof) as cast
// encodes it, with a two-node proof and with the empty proof of a one-account
// tree.
func TestPackLedgerClaimGolden(t *testing.T) {
	alice := [32]byte(mustHexBytes(t, aliceHex))
	proof := []merkle.LedgerNode{
		{Hash: [32]byte(mustHexBytes(t, strings.Repeat("11", 32))), Sum: big.NewInt(5)},
		{Hash: [32]byte(mustHexBytes(t, strings.Repeat("22", 32))), Sum: big.NewInt(900_000_000)},
	}
	data, err := PackLedgerClaim(7, 3, 42, alice, big.NewInt(123_456_789), proof)
	if err != nil {
		t.Fatal(err)
	}
	want := "b7a6b907" +
		"0000000000000000000000000000000000000000000000000000000000000007" +
		"0000000000000000000000000000000000000000000000000000000000000003" +
		"000000000000000000000000000000000000000000000000000000000000002a" +
		aliceHex +
		"00000000000000000000000000000000000000000000000000000000075bcd15" +
		"00000000000000000000000000000000000000000000000000000000000000c0" +
		"0000000000000000000000000000000000000000000000000000000000000002" +
		strings.Repeat("11", 32) +
		"0000000000000000000000000000000000000000000000000000000000000005" +
		strings.Repeat("22", 32) +
		"0000000000000000000000000000000000000000000000000000000035a4e900"
	if got := hex.EncodeToString(data); got != want {
		t.Fatalf("claim calldata\n got %s\nwant %s", got, want)
	}

	data, err = PackLedgerClaim(1, 0, 0, alice, big.NewInt(5), []merkle.LedgerNode{})
	if err != nil {
		t.Fatal(err)
	}
	want = "b7a6b907" +
		"0000000000000000000000000000000000000000000000000000000000000001" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		aliceHex +
		"0000000000000000000000000000000000000000000000000000000000000005" +
		"00000000000000000000000000000000000000000000000000000000000000c0" +
		"0000000000000000000000000000000000000000000000000000000000000000"
	if got := hex.EncodeToString(data); got != want {
		t.Fatalf("depth 0 claim calldata\n got %s\nwant %s", got, want)
	}
}

// APPS-SPEC claim rule 2: an amount that is not a uint256 word is refused,
// naming it.
func TestPackLedgerClaimRefusesNonWords(t *testing.T) {
	alice := [32]byte(mustHexBytes(t, aliceHex))
	if _, err := PackLedgerClaim(1, 0, 0, alice, big.NewInt(-1), nil); err == nil || !strings.HasPrefix(err.Error(), "cumulative: ") {
		t.Fatalf("negative cumulative: %v", err)
	}
	proof := []merkle.LedgerNode{{Sum: big.NewInt(1)}, {Sum: new(big.Int).Lsh(big.NewInt(1), 256)}}
	if _, err := PackLedgerClaim(1, 0, 3, alice, big.NewInt(5), proof); err == nil || !strings.HasPrefix(err.Error(), "proof 1 sum: ") {
		t.Fatalf("a proof sum beyond uint256: %v", err)
	}
}

// APPS-SPEC claim rules 1 and 4: the views and the precompile transfer, as
// cast encodes them.
func TestPackLedgerViewsAndTransfer(t *testing.T) {
	if got := hex.EncodeToString(PackLedgerRoots(7, 3)); got != "0fc3cec0"+hex.EncodeToString(word(7))+hex.EncodeToString(word(3)) {
		t.Fatalf("roots calldata %s", got)
	}
	if got := hex.EncodeToString(PackLedgerClaimed(7, 42)); got != "a54ab457"+hex.EncodeToString(word(7))+hex.EncodeToString(word(42)) {
		t.Fatalf("claimed calldata %s", got)
	}
	if got := hex.EncodeToString(PackEscrowHotkey()); got != "164bf360" {
		t.Fatalf("escrowHotkey calldata %s", got)
	}
	alice := [32]byte(mustHexBytes(t, aliceHex))
	escrow := [32]byte(mustHexBytes(t, strings.Repeat("33", 32)))
	want := "17ce5f62" + aliceHex + strings.Repeat("33", 32) +
		"0000000000000000000000000000000000000000000000000000000000000019" +
		"0000000000000000000000000000000000000000000000000000000000000019" +
		"00000000000000000000000000000000000000000000000000000000000012fd"
	if got := hex.EncodeToString(PackTransferStake(alice, escrow, 25, 4861)); got != want {
		t.Fatalf("transferStake calldata\n got %s\nwant %s", got, want)
	}
}

// APPS-SPEC claim rule 1: the Root tuple roots() returns, and the returns it
// refuses.
func TestDecodeLedgerRoot(t *testing.T) {
	var ret []byte
	ret = append(ret, mustHexBytes(t, strings.Repeat("aa", 32))...)
	ret = append(ret, word(1_000_000_007)...)
	ret = append(ret, word(5)...)
	ret = append(ret, word(12)...)
	ret = append(ret, mustHexBytes(t, strings.Repeat("bb", 32))...)
	root, err := DecodeLedgerRoot(ret)
	if err != nil {
		t.Fatal(err)
	}
	if root.Root != [32]byte(mustHexBytes(t, strings.Repeat("aa", 32))) || root.Total.Int64() != 1_000_000_007 || root.LeafCount != 5 || root.Epoch != 12 ||
		root.LedgerHash != [32]byte(mustHexBytes(t, strings.Repeat("bb", 32))) {
		t.Fatalf("root %+v", root)
	}
	if _, err := DecodeLedgerRoot(ret[:4*32]); err == nil || !strings.Contains(err.Error(), "too short") {
		t.Fatalf("short return: %v", err)
	}
	wide := append([]byte(nil), ret...)
	wide[64] = 1
	if _, err := DecodeLedgerRoot(wide); err == nil || !strings.Contains(err.Error(), "overflows uint64") {
		t.Fatalf("wide leaf count: %v", err)
	}
}

func testClaimedLog(t *testing.T) *Log {
	t.Helper()
	caller := [20]byte(mustHexBytes(t, strings.Repeat("44", 20)))
	data := append(word(3), word(123_456_789)...)
	data = append(data, word(23_456_789)...)
	data = append(data, make([]byte, 12)...)
	data = append(data, caller[:]...)
	return &Log{
		Address: [20]byte(mustHexBytes(t, strings.Repeat("ab", 20))),
		Topics:  [][32]byte{LedgerClaimedTopic, uint64Word(7), uint64Word(42), [32]byte(mustHexBytes(t, aliceHex))},
		Data:    data,
	}
}

// APPS-SPEC claim rule 5: Claimed(noId, rootIndex, index, coldkey, cumulative, amount, caller)
// with every field, and every other log refused.
func TestDecodeLedgerClaimed(t *testing.T) {
	event, ok := DecodeLedgerClaimed(testClaimedLog(t))
	if !ok {
		t.Fatal("the Claimed log did not decode")
	}
	if event.NoId.Int64() != 7 || event.RootIndex.Int64() != 3 || event.Index.Int64() != 42 || event.Coldkey != [32]byte(mustHexBytes(t, aliceHex)) ||
		event.Cumulative.Int64() != 123_456_789 || event.Amount.Int64() != 23_456_789 || event.Caller != [20]byte(mustHexBytes(t, strings.Repeat("44", 20))) {
		t.Fatalf("event %+v", event)
	}
	for name, mutate := range map[string]func(*Log){
		"another event":   func(l *Log) { l.Topics[0] = Keccak256([]byte("Other()")) },
		"three topics":    func(l *Log) { l.Topics = l.Topics[:3] },
		"short data":      func(l *Log) { l.Data = l.Data[:3*32] },
		"anonymous event": func(l *Log) { l.Topics = nil },
	} {
		log := testClaimedLog(t)
		mutate(log)
		if _, ok := DecodeLedgerClaimed(log); ok {
			t.Fatalf("%s decoded as Claimed", name)
		}
	}
}

// APPS-SPEC claim rule 4: vault2's claim refusals decode by name.
func TestDecodeLedgerRevertNames(t *testing.T) {
	for _, name := range []string{"UnknownRoot", "InvalidColdkey", "InvalidProof", "NothingToClaim", "AmountTooLarge", "PoolCapExceeded", "RuntimeAccountingMismatch", "Underfunded", "Reentrancy"} {
		sel := selector(name + "()")
		if got := DecodeRevert(sel[:]); got != name {
			t.Fatalf("%s decoded as %q", name, got)
		}
	}
	if got := DecodeRevert(mustHexBytes(t, "969bf728")); got != "NothingToClaim" {
		t.Fatalf("cast's NothingToClaim selector decoded as %q", got)
	}
}
