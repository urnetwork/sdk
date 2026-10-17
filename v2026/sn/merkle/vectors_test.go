package merkle

// Shared cross-implementation test vectors.
//
// testdata/merkle_vectors.json is the single vector file consumed by:
//   - this package (regenerated in-memory and compared byte-for-byte, so
//     any behavior drift fails TestVectorsFileUpToDate),
//   - the Solidity forge tests (OZ MerkleProof.verify against each root),
//   - the minimal Merkle re-implementation in other UR repos.
//
// Vector inputs are fixed-seed pseudo-random — every key and value is
// derived from keccak256 of a constant tag, never from time or math/rand —
// so regeneration is bit-identical on every platform and Go version:
//
//	key_i   = keccak256("urnetwork/sn/merkle vectors v1|" + name + "|key|" + itoa(i))
//	value_i = uint64(big-endian first 8 bytes of
//	          keccak256("urnetwork/sn/merkle vectors v1|" + name + "|value|" + itoa(i)))
//	          payout: value_i % 10001 (share bps domain 0..10000)
//	          effort: value_i as-is
//
// except effort_5 pins two encoding extremes by hand: leaf 0 has value 0
// (all-zero uint256 word) and leaf 4 has value 2^256-1 (all-0xff word).
//
// Schema: a top-level JSON array of cases
//
//	{name, leafType: "payout"|"effort",
//	 leaves: [{coldkey|pathId: 0x-hex32, value: decimal string}, ...],
//	 root: 0x-hex32,
//	 proofs: [[0x-hex32, ...] per leaf, in input order]}
//
// Run `go test ./merkle -run TestVectorsFileUpToDate -update` to rewrite
// the file after an intentional scheme change (which also requires
// changing the Solidity side!).

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/merkle_vectors.json")

const (
	vectorsPath = "testdata/merkle_vectors.json"
	vectorSeed  = "urnetwork/sn/merkle vectors v1"

	leafTypePayout = "payout"
	leafTypeEffort = "effort"
)

type vectorLeaf struct {
	Coldkey string `json:"coldkey,omitempty"` // payout leaves
	PathID  string `json:"pathId,omitempty"`  // effort leaves
	Value   string `json:"value"`             // decimal uint256
}

type vectorCase struct {
	Name     string       `json:"name"`
	LeafType string       `json:"leafType"`
	Leaves   []vectorLeaf `json:"leaves"`
	Root     string       `json:"root"`
	Proofs   [][]string   `json:"proofs"` // per leaf, input order, leaf-to-root
}

// maxUint256 = 2^256 - 1.
func maxUint256() *big.Int {
	return new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
}

func vectorKey(name string, i int) [32]byte {
	return keccak256([]byte(vectorSeed + "|" + name + "|key|" + strconv.Itoa(i)))
}

func vectorValue(name string, leafType string, i int) *big.Int {
	// effort_5 pins the uint256 encoding extremes.
	if name == "effort_5" && i == 0 {
		return big.NewInt(0)
	}
	if name == "effort_5" && i == 4 {
		return maxUint256()
	}
	h := keccak256([]byte(vectorSeed + "|" + name + "|value|" + strconv.Itoa(i)))
	v := binary.BigEndian.Uint64(h[:8])
	if leafType == leafTypePayout {
		v %= 10001 // share bps 0..10000
	}
	return new(big.Int).SetUint64(v)
}

func buildVectorCase(t *testing.T, name, leafType string, n int) vectorCase {
	t.Helper()
	c := vectorCase{
		Name:     name,
		LeafType: leafType,
		Leaves:   make([]vectorLeaf, n),
		Proofs:   make([][]string, n),
	}
	leaves := make([]Leaf, n)
	for i := 0; i < n; i++ {
		key := vectorKey(name, i)
		value := vectorValue(name, leafType, i)
		switch leafType {
		case leafTypePayout:
			c.Leaves[i] = vectorLeaf{Coldkey: hex32(key), Value: value.String()}
			leaves[i] = PayoutLeaf(key, value)
		case leafTypeEffort:
			c.Leaves[i] = vectorLeaf{PathID: hex32(key), Value: value.String()}
			leaves[i] = EffortLeaf(key, value)
		default:
			t.Fatalf("unknown leaf type %q", leafType)
		}
	}
	tree := mustTree(t, leaves)
	c.Root = hex32(tree.Root())
	for i := range leaves {
		proof, err := tree.ProofAt(i)
		if err != nil {
			t.Fatalf("%s ProofAt(%d): %v", name, i, err)
		}
		hexProof := make([]string, 0, len(proof))
		for _, p := range proof {
			hexProof = append(hexProof, hex32(p))
		}
		c.Proofs[i] = hexProof
	}
	return c
}

func buildAllVectors(t *testing.T) []vectorCase {
	t.Helper()
	return []vectorCase{
		buildVectorCase(t, "payout_1", leafTypePayout, 1),
		buildVectorCase(t, "payout_2", leafTypePayout, 2),
		buildVectorCase(t, "payout_3", leafTypePayout, 3),
		buildVectorCase(t, "payout_5", leafTypePayout, 5),
		buildVectorCase(t, "payout_8", leafTypePayout, 8),
		buildVectorCase(t, "payout_33", leafTypePayout, 33),
		buildVectorCase(t, "effort_5", leafTypeEffort, 5),
		buildVectorCase(t, "effort_33", leafTypeEffort, 33),
	}
}

func marshalVectors(t *testing.T, cases []vectorCase) []byte {
	t.Helper()
	out, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

// TestVectorsFileUpToDate regenerates every vector in-memory and requires
// byte equality with the committed file, so any drift in leaf encoding,
// hashing, sorting, tree shape, or proof layout is caught immediately.
func TestVectorsFileUpToDate(t *testing.T) {
	got := marshalVectors(t, buildAllVectors(t))
	if *update {
		if err := os.MkdirAll(filepath.Dir(vectorsPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(vectorsPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote %s (%d bytes)", vectorsPath, len(got))
		return
	}
	want, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatalf("%v (generate it with: go test ./merkle -run TestVectorsFileUpToDate -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("regenerated vectors differ from %s — the Merkle scheme drifted; if intentional "+
			"(coordinated with the Solidity side), rerun with -update", vectorsPath)
	}
}

// TestVectorFileRoundTrip consumes the committed file exactly the way an
// external implementation (forge test or Go port) would: parse the leaf
// tuples, rebuild leaf hashes and the tree, and check roots and proofs.
func TestVectorFileRoundTrip(t *testing.T) {
	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatalf("%v (generate it with: go test ./merkle -run TestVectorsFileUpToDate -update)", err)
	}
	var cases []vectorCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 8 {
		t.Fatalf("vector file has %d cases, want 8", len(cases))
	}

	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			root := mustHex32(t, c.Root)
			leaves := make([]Leaf, len(c.Leaves))
			for i, vl := range c.Leaves {
				value := mustBig(t, vl.Value)
				switch c.LeafType {
				case leafTypePayout:
					if vl.Coldkey == "" || vl.PathID != "" {
						t.Fatalf("leaf %d: payout leaves carry coldkey only", i)
					}
					leaves[i] = PayoutLeaf(mustHex32(t, vl.Coldkey), value)
				case leafTypeEffort:
					if vl.PathID == "" || vl.Coldkey != "" {
						t.Fatalf("leaf %d: effort leaves carry pathId only", i)
					}
					leaves[i] = EffortLeaf(mustHex32(t, vl.PathID), value)
				default:
					t.Fatalf("unknown leafType %q", c.LeafType)
				}
			}

			tree := mustTree(t, leaves)
			if hex32(tree.Root()) != c.Root {
				t.Fatalf("rebuilt root %s != committed root %s", hex32(tree.Root()), c.Root)
			}
			if len(c.Proofs) != len(leaves) {
				t.Fatalf("%d proofs for %d leaves", len(c.Proofs), len(leaves))
			}
			for i, hexProof := range c.Proofs {
				proof := make([][32]byte, len(hexProof))
				for j, h := range hexProof {
					proof[j] = mustHex32(t, h)
				}
				// The committed proof verifies against the committed root...
				if !Verify(root, leaves[i], proof) {
					t.Fatalf("committed proof %d does not verify", i)
				}
				// ...fails for a mutated leaf...
				bad := leaves[i]
				bad[0] ^= 0x01
				if Verify(root, bad, proof) {
					t.Fatalf("committed proof %d verified a mutated leaf", i)
				}
				// ...and matches the rebuilt tree's proof exactly.
				rebuilt, err := tree.ProofAt(i)
				if err != nil {
					t.Fatal(err)
				}
				if len(rebuilt) != len(proof) {
					t.Fatalf("proof %d: rebuilt length %d != committed %d", i, len(rebuilt), len(proof))
				}
				for j := range rebuilt {
					if rebuilt[j] != proof[j] {
						t.Fatalf("proof %d element %d: rebuilt %s != committed %s",
							i, j, hex32(rebuilt[j]), hexProof[j])
					}
				}
			}
		})
	}
}
