package merkle

// The SDK's port of `urnetwork-ledger-sum-tree-v1` against the vectors the
// sn repository pins its Go twin and LedgerTree.sol to
// (testdata/ledger_v2_vectors.json, copied unchanged). The SDK verifies claim
// proofs only, so the path walks (faultCases) are not ported.

import (
	"encoding/json"
	"math/big"
	"os"
	"testing"
)

const ledgerVectorsPath = "testdata/ledger_v2_vectors.json"

type ledgerVectorFile struct {
	Depths []struct {
		Depth     int    `json:"depth"`
		LeafCount uint64 `json:"leafCount"`
	} `json:"depths"`
	InvalidLeafCounts []uint64 `json:"invalidLeafCounts"`
	LeafHashes        []struct {
		Coldkey string `json:"coldkey"`
		Hash    string `json:"hash"`
		Value   string `json:"value"`
	} `json:"leafHashes"`
	NodeHashes []struct {
		Hash      string `json:"hash"`
		LeftHash  string `json:"leftHash"`
		LeftSum   string `json:"leftSum"`
		RightHash string `json:"rightHash"`
		RightSum  string `json:"rightSum"`
	} `json:"nodeHashes"`
	ProofCases []struct {
		Account   ledgerVectorAccount `json:"account"`
		Error     string              `json:"error"`
		LeafCount uint64              `json:"leafCount"`
		Name      string              `json:"name"`
		Proof     []ledgerVectorNode  `json:"proof"`
		Root      string              `json:"root"`
		Total     string              `json:"total"`
		Valid     bool                `json:"valid"`
	} `json:"proofCases"`
	Scheme string `json:"scheme"`
	Trees  []struct {
		Accounts  []ledgerVectorAccount `json:"accounts"`
		Depth     int                   `json:"depth"`
		LeafCount uint64                `json:"leafCount"`
		Name      string                `json:"name"`
		Proofs    [][]ledgerVectorNode  `json:"proofs"`
		Root      string                `json:"root"`
		Total     string                `json:"total"`
	} `json:"trees"`
}

type ledgerVectorAccount struct {
	Coldkey    string `json:"coldkey"`
	Cumulative string `json:"cumulative"`
	Index      string `json:"index"`
}

type ledgerVectorNode struct {
	Hash string `json:"hash"`
	Sum  string `json:"sum"`
}

func loadLedgerVectors(t *testing.T) *ledgerVectorFile {
	t.Helper()
	raw, err := os.ReadFile(ledgerVectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var file ledgerVectorFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if file.Scheme != LedgerSumTreeScheme {
		t.Fatalf("scheme %q", file.Scheme)
	}
	return &file
}

// vectorUint parses a decimal uint256; the overflow cases use 2^256 − 1.
func vectorUint(t *testing.T, s string) *big.Int {
	t.Helper()
	v, ok := new(big.Int).SetString(s, 10)
	if !ok || !isUint256(v) {
		t.Fatalf("%q is not a decimal uint256", s)
	}
	return v
}

func vectorProof(t *testing.T, nodes []ledgerVectorNode) []LedgerNode {
	t.Helper()
	proof := make([]LedgerNode, len(nodes))
	for k, node := range nodes {
		proof[k] = LedgerNode{Hash: mustHex32(t, node.Hash), Sum: vectorUint(t, node.Sum)}
	}
	return proof
}

// vectorIndex parses an account index; the n5_index_max case uses 2^256 − 1,
// which no uint64 index reaches, so it is clamped to the largest one.
func vectorIndex(t *testing.T, s string) uint64 {
	t.Helper()
	v := vectorUint(t, s)
	if !v.IsUint64() {
		return ^uint64(0)
	}
	return v.Uint64()
}

// GT-1, GT-6, T-1 (SDK port): the depth of every vector leaf count, and the
// refused counts.
func TestLedgerTreeDepthVectors(t *testing.T) {
	file := loadLedgerVectors(t)
	if len(file.Depths) == 0 {
		t.Fatal("no depth vectors")
	}
	for _, c := range file.Depths {
		if d, err := LedgerTreeDepth(c.LeafCount); err != nil || d != c.Depth {
			t.Fatalf("depth(%d) = %d, %v; want %d", c.LeafCount, d, err, c.Depth)
		}
	}
	for _, n := range file.InvalidLeafCounts {
		if _, err := LedgerTreeDepth(n); err != ErrLedgerLeafCount {
			t.Fatalf("depth(%d) error %v, want ErrLedgerLeafCount", n, err)
		}
	}
}

// T-1 (SDK port): leaf and node hashes, including the value extremes 0 and
// 2^128 − 1.
func TestLedgerHashVectors(t *testing.T) {
	file := loadLedgerVectors(t)
	if len(file.LeafHashes) == 0 || len(file.NodeHashes) == 0 {
		t.Fatal("no hash vectors")
	}
	for _, c := range file.LeafHashes {
		if got := LedgerLeafHash(mustHex32(t, c.Coldkey), vectorUint(t, c.Value)); got != mustHex32(t, c.Hash) {
			t.Fatalf("leafHash(%s, %s) = %x, want %s", c.Coldkey, c.Value, got, c.Hash)
		}
	}
	for _, c := range file.NodeHashes {
		got := LedgerNodeHash(mustHex32(t, c.LeftHash), vectorUint(t, c.LeftSum), mustHex32(t, c.RightHash), vectorUint(t, c.RightSum))
		if got != mustHex32(t, c.Hash) {
			t.Fatalf("nodeHash(%+v) = %x", c, got)
		}
	}
}

// GT-2, X-1 (SDK port): every account of every vector tree verifies against its root
// and total, and nowhere else: not at another index, not with another value
// and not against another tree's root.
func TestLedgerTreeProofVectors(t *testing.T) {
	file := loadLedgerVectors(t)
	if len(file.Trees) == 0 {
		t.Fatal("no tree vectors")
	}
	for _, tree := range file.Trees {
		root, total := mustHex32(t, tree.Root), vectorUint(t, tree.Total)
		if d, err := LedgerTreeDepth(tree.LeafCount); err != nil || d != tree.Depth || uint64(len(tree.Accounts)) != tree.LeafCount {
			t.Fatalf("%s: depth %d, %v", tree.Name, d, err)
		}
		for i, account := range tree.Accounts {
			coldkey, value, index := mustHex32(t, account.Coldkey), vectorUint(t, account.Cumulative), vectorIndex(t, account.Index)
			proof := vectorProof(t, tree.Proofs[i])
			if !VerifyLedgerProof(root, total, tree.LeafCount, index, coldkey, value, proof) {
				t.Fatalf("%s: account %d does not verify", tree.Name, i)
			}
			if tree.LeafCount > 1 && VerifyLedgerProof(root, total, tree.LeafCount, index^1, coldkey, value, proof) {
				t.Fatalf("%s: account %d verifies at index %d", tree.Name, i, index^1)
			}
			if VerifyLedgerProof(root, total, tree.LeafCount, index, coldkey, new(big.Int).Add(value, big.NewInt(1)), proof) {
				t.Fatalf("%s: account %d verifies with a higher value", tree.Name, i)
			}
			if VerifyLedgerProof(root, new(big.Int).Add(total, big.NewInt(1)), tree.LeafCount, index, coldkey, value, proof) {
				t.Fatalf("%s: account %d verifies against a higher total", tree.Name, i)
			}
		}
	}
}

// GT-3, T-2, T-4 (SDK port): the proof cases, valid and each mutation. Where
// LedgerTree reverts (InvalidLeafCount, ArithmeticOverflow) the fold is false.
func TestLedgerProofCaseVectors(t *testing.T) {
	file := loadLedgerVectors(t)
	if len(file.ProofCases) == 0 {
		t.Fatal("no proof cases")
	}
	valid := 0
	for _, c := range file.ProofCases {
		got := VerifyLedgerProof(mustHex32(t, c.Root), vectorUint(t, c.Total), c.LeafCount, vectorIndex(t, c.Account.Index),
			mustHex32(t, c.Account.Coldkey), vectorUint(t, c.Account.Cumulative), vectorProof(t, c.Proof))
		if want := c.Valid && c.Error == ""; got != want {
			t.Fatalf("%s: verify = %v, want %v", c.Name, got, want)
		}
		if got {
			valid++
		}
	}
	if valid != 2 {
		t.Fatalf("%d valid proof cases, want n5_valid and n1_valid", valid)
	}
}

// GT-3 (SDK port): inputs that are not uint256 words, which the vectors cannot
// carry, give false and never panic.
func TestLedgerProofRefusesNonWords(t *testing.T) {
	coldkey := [32]byte{1}
	value := big.NewInt(5)
	sibling := LedgerNode{Hash: [32]byte{2}, Sum: big.NewInt(7)}
	root := LedgerNodeHash(LedgerLeafHash(coldkey, value), value, sibling.Hash, sibling.Sum)
	total := big.NewInt(12)
	if !VerifyLedgerProof(root, total, 2, 0, coldkey, value, []LedgerNode{sibling}) {
		t.Fatal("the two-leaf proof does not verify")
	}
	tooWide := new(big.Int).Lsh(big.NewInt(1), 256)
	for name, c := range map[string]struct {
		total *big.Int
		value *big.Int
		sum   *big.Int
	}{
		"nil total":      {nil, value, sibling.Sum},
		"negative total": {big.NewInt(-1), value, sibling.Sum},
		"nil value":      {total, nil, sibling.Sum},
		"wide value":     {total, tooWide, sibling.Sum},
		"nil sum":        {total, value, nil},
		"negative sum":   {total, value, big.NewInt(-7)},
		"wide sum":       {total, value, tooWide},
	} {
		if VerifyLedgerProof(root, c.total, 2, 0, coldkey, c.value, []LedgerNode{{Hash: sibling.Hash, Sum: c.sum}}) {
			t.Fatalf("%s verified", name)
		}
	}
}
