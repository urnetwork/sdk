package evm

import (
	"encoding/hex"
	"math/big"
	"testing"

	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
)

type secpKey = secp.PrivateKey

const (
	goldenDynamicFeeRaw  = "02f8728203b103843b9aca008506fc23ac008301d4c09435353535353535353535353535353535353535358084ce479a1bc001a038667b53e46d66378c098f313f20f7aa0929ee3a0c87649b95e526b1c5acf801a05764e9b81f9f6cdbaabda6c89a1104bb9746438353c2a9150a8c58097df0759f"
	goldenDynamicFeeHash = "2ba899eb8ceaf427eae9fa24d6065483ccc5e2c61eee69cf4939f8c442fad835"
)

func eip155Key(t *testing.T) *secpKey {
	t.Helper()
	key, err := PrivateKeyFromHex("0x4646464646464646464646464646464646464646464646464646464646464646")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// The EIP-155 worked example: nonce 9, 20 gwei, 21000 gas, 1 ether to 0x3535…, chain 1.
func TestLegacyEip155Vector(t *testing.T) {
	key := eip155Key(t)
	to, _ := ParseAddress("0x3535353535353535353535353535353535353535")
	tx := &LegacyTx{Nonce: 9, GasPrice: big.NewInt(20_000_000_000), Gas: 21000, To: to, Value: new(big.Int).Mul(big.NewInt(1_000_000_000), big.NewInt(1_000_000_000))}
	chainId := big.NewInt(1)
	signingHash := tx.SigningHash(chainId)
	if got := hex.EncodeToString(signingHash[:]); got != "daf5a779ae972f972197303d7b574746c7ef83eadac0f2791ad23db92e4c8e53" {
		t.Fatalf("signing hash = %s", got)
	}
	raw, _, err := tx.Signed(chainId, key)
	if err != nil {
		t.Fatal(err)
	}
	want := "f86c098504a817c800825208943535353535353535353535353535353535353535880de0b6b3a76400008025a028ef61340bd939bc2195fe537567866003e1a15d3c71ff63e1590620aa636276a067cbe9d8997f761aecb703304b3800ccf555c9f3dc64214b297fb1966a3b6d83"
	if got := hex.EncodeToString(raw); got != want {
		t.Fatalf("raw tx\n got %s\nwant %s", got, want)
	}
	sig, err := Sign(tx.SigningHash(chainId), key)
	if err != nil {
		t.Fatal(err)
	}
	from, err := Recover(tx.SigningHash(chainId), sig)
	if err != nil {
		t.Fatal(err)
	}
	if from != AddressOf(key) {
		t.Fatal("recovered signer mismatch")
	}
}

// Golden produced with go-ethereum types.SignNewTx (LondonSigner) for the
// same key: chain 945, nonce 3, tip 1 gwei, cap 30 gwei, gas 120000, to
// 0x3535…, value 0, data 0xce479a1b.
func TestDynamicFeeGolden(t *testing.T) {
	key := eip155Key(t)
	to, _ := ParseAddress("0x3535353535353535353535353535353535353535")
	tx := &DynamicFeeTx{Nonce: 3, MaxPriorityFeePerGas: big.NewInt(1_000_000_000), MaxFeePerGas: big.NewInt(30_000_000_000), Gas: 120000, To: to, Value: big.NewInt(0), Data: []byte{0xce, 0x47, 0x9a, 0x1b}}
	chainId := big.NewInt(945)
	raw, hash, err := tx.Signed(chainId, key)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(raw); got != goldenDynamicFeeRaw {
		t.Fatalf("raw tx\n got %s\nwant %s", got, goldenDynamicFeeRaw)
	}
	if got := hex.EncodeToString(hash[:]); got != goldenDynamicFeeHash {
		t.Fatalf("hash %s want %s", got, goldenDynamicFeeHash)
	}
	sig, _ := Sign(tx.SigningHash(chainId), key)
	from, err := Recover(tx.SigningHash(chainId), sig)
	if err != nil || from != AddressOf(key) {
		t.Fatalf("recover: %v", err)
	}
}
