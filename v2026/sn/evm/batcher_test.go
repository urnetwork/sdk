package evm

// The STClaimBatcher encodings a batched v2 claim uses. The goldens are the
// selector and topics of sn/evm/abi/STClaimBatcher.abi.json (cast sig, cast
// keccak) and calldata from Foundry's cast.

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/urnetwork/sdk/v2026/sn/merkle"
)

func TestClaimBatcherSelectorsAndTopics(t *testing.T) {
	if got := hex.EncodeToString(claimManySelector[:]); got != "d77e363f" {
		t.Fatalf("claimMany selector %s", got)
	}
	for name, c := range map[string]struct {
		got  [32]byte
		want string
	}{
		"ClaimPaid":          {ClaimPaidTopic, "b0727183796e52c5bb00a257c1eb36d8e94dbee5d6d87e259287e3372ae39ace"},
		"ClaimRefused":       {ClaimRefusedTopic, "f869308b71c9a01aff5b8d5408027b38b9076abea371ee96b2c875d292caf693"},
		"ClaimsNotAttempted": {ClaimsNotAttemptedTopic, "2f7b166a2d86f29c730e41cba281e1874a958f8195859df7db757d744e573a9c"},
	} {
		if got := hex.EncodeToString(c.got[:]); got != c.want {
			t.Fatalf("%s topic %s, want %s", name, got, c.want)
		}
	}
	if got := hex.EncodeToString(ClaimBatcherRuntimeHash[:]); got != "2964c2882e0e4f85c2295563da92ead2fa8669d0045059eefbefa187ffd22c26" {
		t.Fatalf("runtime hash %s", got)
	}
}

// claimMany(vault2, [pool 7's claim with a two-node proof, a capture the flag
// pool's claim at depth 0], 2,600,000) as cast encodes it.
func TestPackClaimManyGolden(t *testing.T) {
	alice := [32]byte(mustHexBytes(t, strings.Repeat("a1", 32)))
	vault, err := ParseAddress("0xe4865eb1bebdf5c6a10c9d37f3ccdd59aee8f20c")
	if err != nil {
		t.Fatal(err)
	}
	claims := []*LedgerClaim{
		{NoId: 7, RootIndex: 3, Index: 42, Coldkey: alice, Cumulative: big.NewInt(123_456_789), Proof: []merkle.LedgerNode{
			{Hash: [32]byte(mustHexBytes(t, strings.Repeat("11", 32))), Sum: big.NewInt(5)},
			{Hash: [32]byte(mustHexBytes(t, strings.Repeat("22", 32))), Sum: big.NewInt(900_000_000)},
		}},
		{NoId: 1<<32 + 1, RootIndex: 0, Index: 0, Coldkey: alice, Cumulative: big.NewInt(5), Proof: []merkle.LedgerNode{}},
	}
	data, err := PackClaimMany(vault, claims, 2_600_000)
	if err != nil {
		t.Fatal(err)
	}
	want := "d77e363f" +
		"000000000000000000000000e4865eb1bebdf5c6a10c9d37f3ccdd59aee8f20c" +
		"0000000000000000000000000000000000000000000000000000000000000060" +
		"000000000000000000000000000000000000000000000000000000000027ac40" +
		"0000000000000000000000000000000000000000000000000000000000000002" +
		"0000000000000000000000000000000000000000000000000000000000000040" +
		"00000000000000000000000000000000000000000000000000000000000001a0" +
		"0000000000000000000000000000000000000000000000000000000000000007" +
		"0000000000000000000000000000000000000000000000000000000000000003" +
		"000000000000000000000000000000000000000000000000000000000000002a" +
		strings.Repeat("a1", 32) +
		"00000000000000000000000000000000000000000000000000000000075bcd15" +
		"00000000000000000000000000000000000000000000000000000000000000c0" +
		"0000000000000000000000000000000000000000000000000000000000000002" +
		strings.Repeat("11", 32) +
		"0000000000000000000000000000000000000000000000000000000000000005" +
		strings.Repeat("22", 32) +
		"0000000000000000000000000000000000000000000000000000000035a4e900" +
		"0000000000000000000000000000000000000000000000000000000100000001" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		strings.Repeat("a1", 32) +
		"0000000000000000000000000000000000000000000000000000000000000005" +
		"00000000000000000000000000000000000000000000000000000000000000c0" +
		"0000000000000000000000000000000000000000000000000000000000000000"
	if got := hex.EncodeToString(data); got != want {
		t.Fatalf("claimMany calldata\n got %s\nwant %s", got, want)
	}

	// an amount that is not a uint256 word is refused, naming its item
	claims[1].Cumulative = big.NewInt(-1)
	if _, err := PackClaimMany(vault, claims, 2_600_000); err == nil || !strings.HasPrefix(err.Error(), "claim 1: cumulative: ") {
		t.Fatalf("a negative cumulative: %v", err)
	}
}

func TestDecodeClaimManyAttempted(t *testing.T) {
	if attempted, err := DecodeClaimManyAttempted(word(2)); err != nil || attempted != 2 {
		t.Fatalf("attempted %d, %v", attempted, err)
	}
	for _, ret := range [][]byte{nil, word(2)[:31], append(word(2), word(3)...)} {
		if _, err := DecodeClaimManyAttempted(ret); err == nil {
			t.Fatalf("decoded %x", ret)
		}
	}
	if _, err := DecodeClaimManyAttempted(mustHexBytes(t, strings.Repeat("ff", 32))); err == nil {
		t.Fatal("decoded an attempted count beyond uint64")
	}
}

func testBatchLog(batcher [20]byte, topic [32]byte, words ...[]byte) *Log {
	var data []byte
	for _, w := range words {
		data = append(data, w...)
	}
	return &Log{Address: batcher, Topics: [][32]byte{topic}, Data: data}
}

// The outcome of each item from the batcher's events, and the receipts that
// do not report every item in order.
func TestClaimBatchOutcomes(t *testing.T) {
	batcher := [20]byte{19: 0xb2}
	other := [20]byte{19: 0x99}
	selectorWord := append(mustHexBytes(t, "deadbeef"), make([]byte, 28)...)
	paid := testBatchLog(batcher, ClaimPaidTopic, word(0), word(100))
	refused := testBatchLog(batcher, ClaimRefusedTopic, word(1), selectorWord)
	rest := testBatchLog(batcher, ClaimsNotAttemptedTopic, word(2))
	// another contract's log of the same shape is skipped
	foreign := testBatchLog(other, ClaimPaidTopic, word(7), word(1))

	outcomes, err := ClaimBatchOutcomes(&Receipt{Logs: []*Log{foreign, paid, refused, rest}}, batcher, 4)
	if err != nil {
		t.Fatal(err)
	}
	if outcomes[0].Outcome != ClaimBatchPaid || outcomes[0].Amount.Int64() != 100 ||
		outcomes[1].Outcome != ClaimBatchRefused || hex.EncodeToString(outcomes[1].Selector[:]) != "deadbeef" ||
		outcomes[2].Outcome != ClaimBatchNotAttempted || outcomes[3].Outcome != ClaimBatchNotAttempted || outcomes[3].Item != 3 {
		t.Fatalf("outcomes %+v %+v %+v %+v", outcomes[0], outcomes[1], outcomes[2], outcomes[3])
	}

	for name, logs := range map[string][]*Log{
		"an item missing":        {paid, rest},
		"out of order":           {refused, paid},
		"too few":                {paid, refused},
		"an event after the end": {paid, refused, rest, testBatchLog(batcher, ClaimPaidTopic, word(3), word(1))},
		"a malformed event":      {paid, testBatchLog(batcher, ClaimRefusedTopic, word(1))},
		"another event":          {paid, testBatchLog(batcher, LedgerClaimedTopic, word(1), word(1))},
	} {
		if _, err := ClaimBatchOutcomes(&Receipt{Logs: logs}, batcher, 3); err == nil {
			t.Fatalf("%s: read", name)
		}
	}
	// every item paid, and none at all
	if outcomes, err := ClaimBatchOutcomes(&Receipt{Logs: []*Log{paid, testBatchLog(batcher, ClaimPaidTopic, word(1), word(5))}}, batcher, 2); err != nil || outcomes[1].Amount.Int64() != 5 {
		t.Fatalf("all paid: %+v, %v", outcomes, err)
	}
	if outcomes, err := ClaimBatchOutcomes(&Receipt{Logs: []*Log{testBatchLog(batcher, ClaimsNotAttemptedTopic, word(0))}}, batcher, 2); err != nil || outcomes[0].Outcome != ClaimBatchNotAttempted || outcomes[1].Outcome != ClaimBatchNotAttempted {
		t.Fatalf("none attempted: %+v, %v", outcomes, err)
	}
}
