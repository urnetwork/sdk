package evm

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/urnetwork/sdk/sn/merkle"
)

const aliceHex = "d43593c715fdd31c61141abd04a99fd6822c8558854ccde39a5684e7a56da27d"

// The same golden the sn miner pins (sn/miner/onchain/calldata_test.go).
var goldenClaimCalldata = "0xce479a1b" +
	"0000000000000000000000000000000000000000000000000000000000000007" +
	"0000000000000000000000000000000000000000000000000000000000000003" +
	aliceHex +
	"00000000000000000000000000000000000000000000000000000000000004d2" +
	"00000000000000000000000000000000000000000000000000000000000000a0" +
	"0000000000000000000000000000000000000000000000000000000000000002" +
	strings.Repeat("11", 32) +
	strings.Repeat("22", 32)

func TestSelectors(t *testing.T) {
	if got := hex.EncodeToString(claimSelector[:]); got != "ce479a1b" {
		t.Fatalf("claim selector = %s", got)
	}
	if got := hex.EncodeToString(errorStringSelector[:]); got != "08c379a0" {
		t.Fatalf("Error(string) selector = %s", got)
	}
}

func TestPackClaimGolden(t *testing.T) {
	var coldkey [32]byte
	b, _ := hex.DecodeString(aliceHex)
	copy(coldkey[:], b)
	proof := [][32]byte{{}, {}}
	for i := range proof[0] {
		proof[0][i] = 0x11
		proof[1][i] = 0x22
	}
	got, err := PackClaim(big.NewInt(7), big.NewInt(3), coldkey, big.NewInt(1234), proof)
	if err != nil {
		t.Fatal(err)
	}
	if gotHex := "0x" + hex.EncodeToString(got); gotHex != goldenClaimCalldata {
		t.Fatalf("golden mismatch\n got: %s\nwant: %s", gotHex, goldenClaimCalldata)
	}
}

func TestPayoutLeafMatchesMerklePackage(t *testing.T) {
	var coldkey [32]byte
	b, _ := hex.DecodeString(aliceHex)
	copy(coldkey[:], b)
	got, err := PayoutLeaf(coldkey, big.NewInt(1234))
	if err != nil {
		t.Fatal(err)
	}
	want := merkle.PayoutLeaf(coldkey, big.NewInt(1234))
	if !bytes.Equal(got[:], want[:]) {
		t.Fatalf("PayoutLeaf differs from merkle.PayoutLeaf: %x vs %x", got, want)
	}
}

func TestClaimKey(t *testing.T) {
	var coldkey [32]byte
	coldkey[31] = 1
	got, err := ClaimKey(big.NewInt(3), coldkey)
	if err != nil {
		t.Fatal(err)
	}
	var noId [32]byte
	noId[31] = 3
	want := Keccak256(noId[:], coldkey[:])
	if got != want {
		t.Fatal("claim key mismatch")
	}
}

func TestDecodeEntitlement(t *testing.T) {
	var ret []byte
	root := bytes.Repeat([]byte{0xaa}, 32)
	artifact := bytes.Repeat([]byte{0xbb}, 32)
	ret = append(ret, root...)
	ret = append(ret, artifact...)
	w := uint64Word(1000)
	ret = append(ret, w[:]...)
	w = uint64Word(900)
	ret = append(ret, w[:]...)
	w = uint64Word(50)
	ret = append(ret, w[:]...)
	w = uint64Word(123456)
	ret = append(ret, w[:]...)
	w = uint64Word(2)
	ret = append(ret, w[:]...)
	e, err := DecodeEntitlement(ret)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(e.PayoutRoot[:], root) || !bytes.Equal(e.ArtifactHash[:], artifact) {
		t.Fatal("hashes")
	}
	if e.Funded.Int64() != 1000 || e.Total.Int64() != 900 || e.Claimed.Int64() != 50 || e.ExpiryBlock != 123456 || e.Status != StatusFinalized {
		t.Fatalf("decoded %+v", e)
	}
	if _, err := DecodeEntitlement(ret[:100]); err == nil {
		t.Fatal("short return accepted")
	}
}

func TestDecodeRootCommitment(t *testing.T) {
	var ret []byte
	ret = append(ret, bytes.Repeat([]byte{0x01}, 32)...)
	ret = append(ret, bytes.Repeat([]byte{0x02}, 32)...)
	addrWord := make([]byte, 32)
	for i := 12; i < 32; i++ {
		addrWord[i] = 0xcc
	}
	ret = append(ret, addrWord...)
	w := uint64Word(77)
	ret = append(ret, w[:]...)
	c, err := DecodeRootCommitment(ret)
	if err != nil {
		t.Fatal(err)
	}
	if c.CommitBlock != 77 || c.Committer != [20]byte{0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc, 0xcc} {
		t.Fatalf("decoded %+v", c)
	}
}

func TestDecodeRevert(t *testing.T) {
	// Error("boom")
	var data []byte
	data = append(data, errorStringSelector[:]...)
	w := uint64Word(32)
	data = append(data, w[:]...)
	w = uint64Word(4)
	data = append(data, w[:]...)
	data = append(data, []byte("boom")...)
	data = append(data, make([]byte, 28)...)
	if got := DecodeRevert(data); got != "boom" {
		t.Fatalf("Error(string) = %q", got)
	}
	sel := selector("AlreadyClaimed()")
	if got := DecodeRevert(sel[:]); got != "AlreadyClaimed" {
		t.Fatalf("custom error = %q", got)
	}
	if got := DecodeRevert(nil); got != "reverted" {
		t.Fatalf("empty = %q", got)
	}
}

func TestParseUint256(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"0", 0, true}, {"12", 12, true}, {"0x1f", 31, true}, {"0X1f", 31, true},
		{"-1", 0, false}, {"", 0, false}, {"zz", 0, false},
	} {
		v, err := ParseUint256(tc.in)
		if (err == nil) != tc.ok {
			t.Fatalf("%q: err=%v", tc.in, err)
		}
		if tc.ok && v.Int64() != tc.want {
			t.Fatalf("%q = %s", tc.in, v)
		}
	}
	if _, err := ParseUint256("0x1" + strings.Repeat("0", 64)); err == nil {
		t.Fatal("overflow accepted")
	}
}
