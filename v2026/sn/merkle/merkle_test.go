package merkle

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"testing"
)

// --- shared test helpers ---

func mustHex32(t *testing.T, s string) [32]byte {
	t.Helper()
	if !strings.HasPrefix(s, "0x") || len(s) != 66 {
		t.Fatalf("bad hex32 %q", s)
	}
	raw, err := hex.DecodeString(s[2:])
	if err != nil {
		t.Fatalf("bad hex32 %q: %v", s, err)
	}
	var out [32]byte
	copy(out[:], raw)
	return out
}

func hex32(b [32]byte) string {
	return "0x" + hex.EncodeToString(b[:])
}

func mustBig(t *testing.T, s string) *big.Int {
	t.Helper()
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad decimal %q", s)
	}
	return v
}

// testLeaves derives n deterministic, distinct payout leaves.
func testLeaves(tag string, n int) []Leaf {
	leaves := make([]Leaf, n)
	for i := range leaves {
		key := keccak256([]byte("merkle-test|" + tag + "|" + strconv.Itoa(i)))
		leaves[i] = PayoutLeaf(key, big.NewInt(int64(i)))
	}
	return leaves
}

func mustTree(t *testing.T, leaves []Leaf) *Tree {
	t.Helper()
	tree, err := NewTree(leaves)
	if err != nil {
		t.Fatalf("NewTree(%d leaves): %v", len(leaves), err)
	}
	return tree
}

// --- keccak / encoding primitives ---

// TestKeccak256KnownAnswers anchors the hash primitive to two canonical
// legacy Keccak-256 values so a swap to NIST SHA3-256 (different padding)
// fails loudly:
//   - keccak256("") — the EVM empty code hash, Ethereum Yellow Paper.
//   - keccak256("abc") — the classic pre-NIST Keccak-256 test vector
//     (SHA3-256("abc") differs: 0x3a985da7...).
func TestKeccak256KnownAnswers(t *testing.T) {
	if got := hex32(keccak256(nil)); got != "0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470" {
		t.Fatalf("keccak256(\"\") = %s", got)
	}
	if got := hex32(keccak256([]byte("abc"))); got != "0x4e03657aea45a94fc7d47ba826c8d667c0d1e6e33a64a036ec44f58fa12d6c45" {
		t.Fatalf("keccak256(\"abc\") = %s", got)
	}
}

// TestLeafPayloadLayout pins the abi.encode(bytes32, uint256) layout:
// 32-byte key word, then the value as a 32-byte big-endian word.
func TestLeafPayloadLayout(t *testing.T) {
	var key [32]byte
	for i := range key {
		key[i] = byte(0xA0 + i%16)
	}

	cases := []struct {
		value *big.Int
		word  func() [32]byte
	}{
		{big.NewInt(0), func() (w [32]byte) { return }},
		{big.NewInt(1), func() (w [32]byte) { w[31] = 1; return }},
		{new(big.Int).SetUint64(0x0102030405060708), func() (w [32]byte) {
			copy(w[24:], []byte{1, 2, 3, 4, 5, 6, 7, 8})
			return
		}},
		{new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)), func() (w [32]byte) {
			for i := range w {
				w[i] = 0xff
			}
			return
		}},
	}
	for _, c := range cases {
		word := c.word()
		payload := append(append([]byte{}, key[:]...), word[:]...)
		want := LeafFromPayload(payload)
		if got := PayoutLeaf(key, c.value); got != want {
			t.Errorf("PayoutLeaf(key, %s) != LeafFromPayload(manual 64-byte payload)", c.value)
		}
		// Effort leaves are the same (bytes32, uint256) tuple shape.
		if got := EffortLeaf(key, c.value); got != want {
			t.Errorf("EffortLeaf(key, %s) != manual payload hash", c.value)
		}
	}

	// The double hash: leaf = keccak256(keccak256(payload)), not a single hash.
	payload := append(append([]byte{}, key[:]...), make([]byte, 32)...)
	single := keccak256(payload)
	if Leaf(single) == LeafFromPayload(payload) {
		t.Fatal("LeafFromPayload must double-hash the payload")
	}
	if LeafFromPayload(payload) != Leaf(keccak256(single[:])) {
		t.Fatal("LeafFromPayload != keccak256(keccak256(payload))")
	}
}

func TestLeafValuePanics(t *testing.T) {
	var key [32]byte
	for name, v := range map[string]*big.Int{
		"nil":      nil,
		"negative": big.NewInt(-1),
		"overflow": new(big.Int).Lsh(big.NewInt(1), 256), // 2^256
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("PayoutLeaf(key, %s) did not panic", name)
				}
			}()
			PayoutLeaf(key, v)
		})
	}
	// 2^256 - 1 is the largest legal value and must not panic.
	PayoutLeaf(key, new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)))
}

// --- construction edge cases ---

func TestEmptyInput(t *testing.T) {
	if _, err := NewTree(nil); !errors.Is(err, ErrEmptyTree) {
		t.Fatalf("NewTree(nil) err = %v, want ErrEmptyTree", err)
	}
	if _, err := NewTree([]Leaf{}); !errors.Is(err, ErrEmptyTree) {
		t.Fatalf("NewTree([]) err = %v, want ErrEmptyTree", err)
	}
}

func TestSingleLeaf(t *testing.T) {
	leaf := testLeaves("single", 1)[0]
	tree := mustTree(t, []Leaf{leaf})
	if tree.Root() != [32]byte(leaf) {
		t.Fatalf("single-leaf root = %s, want the leaf hash %s", hex32(tree.Root()), hex32(leaf))
	}
	proof, err := tree.Proof(leaf)
	if err != nil {
		t.Fatal(err)
	}
	if len(proof) != 0 {
		t.Fatalf("single-leaf proof has %d elements, want 0", len(proof))
	}
	if !Verify(tree.Root(), leaf, proof) {
		t.Fatal("single-leaf proof does not verify")
	}
}

func TestDuplicateLeaves(t *testing.T) {
	leaves := testLeaves("dup", 5)
	leaves = append(leaves, leaves[2]) // duplicate in the middle of sorted order
	if _, err := NewTree(leaves); !errors.Is(err, ErrDuplicateLeaf) {
		t.Fatalf("NewTree with duplicate err = %v, want ErrDuplicateLeaf", err)
	}
	if _, err := NewTree([]Leaf{leaves[0], leaves[0]}); !errors.Is(err, ErrDuplicateLeaf) {
		t.Fatalf("NewTree with two identical leaves err = %v, want ErrDuplicateLeaf", err)
	}
}

func TestProofLookupErrors(t *testing.T) {
	leaves := testLeaves("lookup", 4)
	tree := mustTree(t, leaves)
	if _, err := tree.Proof(testLeaves("other", 1)[0]); !errors.Is(err, ErrUnknownLeaf) {
		t.Fatalf("Proof(unknown) err = %v, want ErrUnknownLeaf", err)
	}
	for _, i := range []int{-1, 4, 100} {
		if _, err := tree.ProofAt(i); !errors.Is(err, ErrIndexOutOfRange) {
			t.Fatalf("ProofAt(%d) err = %v, want ErrIndexOutOfRange", i, err)
		}
	}
	if tree.Len() != 4 {
		t.Fatalf("Len() = %d, want 4", tree.Len())
	}
}

// --- proof correctness properties ---

func TestAllProofsVerify(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 16, 17, 33, 100} {
		leaves := testLeaves(fmt.Sprintf("size-%d", n), n)
		tree := mustTree(t, leaves)
		root := tree.Root()
		for i, leaf := range leaves {
			byIndex, err := tree.ProofAt(i)
			if err != nil {
				t.Fatalf("n=%d ProofAt(%d): %v", n, i, err)
			}
			byLeaf, err := tree.Proof(leaf)
			if err != nil {
				t.Fatalf("n=%d Proof(leaf %d): %v", n, i, err)
			}
			if len(byIndex) != len(byLeaf) {
				t.Fatalf("n=%d leaf %d: ProofAt and Proof disagree", n, i)
			}
			for j := range byIndex {
				if byIndex[j] != byLeaf[j] {
					t.Fatalf("n=%d leaf %d: ProofAt and Proof disagree at element %d", n, i, j)
				}
			}
			if !Verify(root, leaf, byIndex) {
				t.Fatalf("n=%d leaf %d: proof does not verify", n, i)
			}
		}
	}
}

// lcgPermute deterministically permutes a copy of leaves with a
// Fisher-Yates shuffle driven by an explicit LCG (no math/rand, so the
// sequence can never drift across Go versions).
func lcgPermute(leaves []Leaf, seed uint64) []Leaf {
	out := append([]Leaf(nil), leaves...)
	state := seed
	next := func() uint64 {
		state = state*6364136223846793005 + 1442695040888963407
		return state >> 33
	}
	for i := len(out) - 1; i > 0; i-- {
		j := int(next() % uint64(i+1))
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func TestRootIndependentOfInputOrder(t *testing.T) {
	leaves := testLeaves("order", 33)
	want := mustTree(t, leaves).Root()

	permutations := map[string][]Leaf{
		"reversed": func() []Leaf {
			r := append([]Leaf(nil), leaves...)
			for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
				r[i], r[j] = r[j], r[i]
			}
			return r
		}(),
		"shuffle-1": lcgPermute(leaves, 1),
		"shuffle-2": lcgPermute(leaves, 0xDEADBEEF),
	}
	for name, perm := range permutations {
		tree := mustTree(t, perm)
		if tree.Root() != want {
			t.Errorf("%s: root %s differs from original %s", name, hex32(tree.Root()), hex32(want))
		}
		// ProofAt must track the *caller's* order, not the sorted order.
		for i, leaf := range perm {
			proof, err := tree.ProofAt(i)
			if err != nil {
				t.Fatalf("%s ProofAt(%d): %v", name, i, err)
			}
			if !Verify(want, leaf, proof) {
				t.Fatalf("%s: proof at permuted index %d does not verify", name, i)
			}
		}
	}
}

func TestVerifyRejectsMutations(t *testing.T) {
	for _, n := range []int{2, 5, 8, 33} { // includes odd sizes with promoted nodes
		leaves := testLeaves(fmt.Sprintf("mutate-%d", n), n)
		tree := mustTree(t, leaves)
		root := tree.Root()
		for i, leaf := range leaves {
			proof, err := tree.ProofAt(i)
			if err != nil {
				t.Fatal(err)
			}
			if !Verify(root, leaf, proof) {
				t.Fatalf("n=%d leaf %d: baseline proof does not verify", n, i)
			}

			// Mutated leaf.
			badLeaf := leaf
			badLeaf[7] ^= 0x01
			if Verify(root, badLeaf, proof) {
				t.Errorf("n=%d leaf %d: mutated leaf verified", n, i)
			}
			// Mutated proof element (each position).
			for j := range proof {
				badProof := append([][32]byte(nil), proof...)
				badProof[j][31] ^= 0x80
				if Verify(root, leaf, badProof) {
					t.Errorf("n=%d leaf %d: proof with mutated element %d verified", n, i, j)
				}
			}
			// Truncated proof.
			if len(proof) > 0 && Verify(root, leaf, proof[:len(proof)-1]) {
				t.Errorf("n=%d leaf %d: truncated proof verified", n, i)
			}
			// Extended proof.
			if Verify(root, leaf, append(append([][32]byte(nil), proof...), root)) {
				t.Errorf("n=%d leaf %d: extended proof verified", n, i)
			}
			// Wrong root.
			badRoot := root
			badRoot[0] ^= 0xff
			if Verify(badRoot, leaf, proof) {
				t.Errorf("n=%d leaf %d: wrong root verified", n, i)
			}
			// Proof spliced onto a different leaf.
			if n > 1 {
				other := leaves[(i+1)%n]
				if Verify(root, other, proof) {
					t.Errorf("n=%d: leaf %d's proof verified for leaf %d", n, i, (i+1)%n)
				}
			}
		}
	}
}

// TestPromotedNodeShape pins the canonical odd-node rule directly: with
// three sorted leaf hashes a<b<c, root = hashPair(hashPair(a,b), c) — c is
// promoted, never re-paired early.
func TestPromotedNodeShape(t *testing.T) {
	leaves := testLeaves("shape", 3)
	tree := mustTree(t, leaves)

	sorted := append([]Leaf(nil), leaves...)
	for i := 0; i < len(sorted); i++ { // tiny insertion sort by hash bytes
		for j := i; j > 0 && hex32(sorted[j-1]) > hex32(sorted[j]); j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	want := hashPair(hashPair(sorted[0], sorted[1]), sorted[2])
	if tree.Root() != want {
		t.Fatalf("3-leaf root = %s, want hashPair(hashPair(a,b), c) = %s", hex32(tree.Root()), hex32(want))
	}

	// And for 5 leaves: root = hashPair(hashPair(hashPair(a,b), hashPair(c,d)), e).
	leaves5 := testLeaves("shape5", 5)
	tree5 := mustTree(t, leaves5)
	sorted5 := append([]Leaf(nil), leaves5...)
	for i := 0; i < len(sorted5); i++ {
		for j := i; j > 0 && hex32(sorted5[j-1]) > hex32(sorted5[j]); j-- {
			sorted5[j-1], sorted5[j] = sorted5[j], sorted5[j-1]
		}
	}
	want5 := hashPair(
		hashPair(hashPair(sorted5[0], sorted5[1]), hashPair(sorted5[2], sorted5[3])),
		sorted5[4],
	)
	if tree5.Root() != want5 {
		t.Fatalf("5-leaf root = %s, want promoted-odd shape %s", hex32(tree5.Root()), hex32(want5))
	}
}

func TestHashPairIsSortedAndCommutative(t *testing.T) {
	a := keccak256([]byte("a"))
	b := keccak256([]byte("b"))
	if hashPair(a, b) != hashPair(b, a) {
		t.Fatal("hashPair is not commutative")
	}
	lo, hi := a, b
	if hex32(lo) > hex32(hi) {
		lo, hi = hi, lo
	}
	if hashPair(a, b) != keccak256(lo[:], hi[:]) {
		t.Fatal("hashPair does not hash the ascending-sorted concatenation")
	}
}
