package subtensor

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math/bits"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/blake2b"

	"github.com/urnetwork/sdk/sn/evm"
)

// Metadata captured read-only from the public endpoints of mainnet and testnet
// on 2026-11-02. Both served these same bytes at spec 486.
const (
	runtime486MetadataSize   = 367995
	runtime486MetadataSha256 = "142e2f2735d9585567fd71e080554a2a0ec10d4ad0bcedb4993e87e473ddd852"
)

func runtime486MetadataBytes(t *testing.T) []byte {
	t.Helper()
	encoded, err := os.ReadFile("testdata/runtime486-metadata.scale.gz.base64")
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := base64.StdEncoding.DecodeString(string(encoded))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	if len(raw) != runtime486MetadataSize || hex.EncodeToString(digest[:]) != runtime486MetadataSha256 {
		t.Fatalf("the fixture is not the captured metadata: %d bytes, %x", len(raw), digest)
	}
	return raw
}

// A fresh parse each call: the refusal tests change what they get.
func runtime486Metadata(t *testing.T) *Metadata {
	t.Helper()
	meta, err := ParseMetadata(runtime486MetadataBytes(t))
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

// The vectors substrate-interface and py-scale-codec wrote for these inputs,
// over the same metadata. The sdk has no part in making them.
type testVectorFile struct {
	MetadataSha256     string       `json:"metadata_sha256"`
	Genesis            string       `json:"genesis"`
	SpecVersion        uint32       `json:"spec_version"`
	TransactionVersion uint32       `json:"transaction_version"`
	Vectors            []testVector `json:"vectors"`
}

type testVector struct {
	Name          string `json:"name"`
	CallFunction  string `json:"call_function"`
	BirthBlock    uint64 `json:"birth_block"`
	BirthHash     string `json:"birth_hash"`
	Nonce         uint32 `json:"nonce"`
	Signer        string `json:"signer"`
	Call          string `json:"call"`
	Payload       string `json:"payload"`
	Extrinsic     string `json:"extrinsic"`
	ExtrinsicHash string `json:"extrinsic_hash"`
	Signature     string `json:"signature"`
}

func testUnhex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func testHash32(t *testing.T, value string) [32]byte {
	t.Helper()
	decoded := testUnhex(t, value)
	if len(decoded) != 32 {
		t.Fatalf("%q is not 32 bytes", value)
	}
	return [32]byte(decoded)
}

func testVectors(t *testing.T) testVectorFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var file testVectorFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if file.MetadataSha256 != runtime486MetadataSha256 {
		t.Fatalf("the vectors were made from other metadata: %s", file.MetadataSha256)
	}
	if len(file.Vectors) != 5 {
		t.Fatalf("%d vectors", len(file.Vectors))
	}
	return file
}

// The typed inputs behind each vector, as make_vectors.py was given them.
func testVectorCall(t *testing.T, vector testVector) *Call {
	t.Helper()
	escrow := testHash32(t, "a838b2d93ad97cb0fcc91f3bda505ae2bc221b348e47eeb07fd9d0b2ad8dc24b")
	other := testHash32(t, "8eaf04151687736326c9fea17e25fc5287613693c912909cb226aa4794f26a48")
	var call *Call
	var err error
	switch {
	case strings.Contains(vector.Name, "first transaction"):
		call, err = NewRemoveStakeLimit(escrow, 25, 1_000_000_000, 4_070_000)
	case strings.Contains(vector.Name, "two bytes"):
		call, err = NewRemoveStakeLimit(escrow, 25, 123_456_789_012, 4_321)
	case strings.Contains(vector.Name, "four bytes"):
		call, err = NewRemoveStakeLimit(other, 25, 18_000_000_000_000_000_000, 18_000_000_000_000_000_000)
	case vector.Name == "register_limit":
		call, err = NewRegisterLimit(25, other, 550_000)
	case vector.Name == "set_commitment":
		call, err = NewSetFleetCommitment(25, [32]byte(bytes.Repeat([]byte{0xcd}, 32)))
	default:
		t.Fatalf("no inputs for vector %q", vector.Name)
	}
	if err != nil {
		t.Fatal(err)
	}
	return call
}

func testSigningContext(t *testing.T, file testVectorFile, vector testVector) SigningContext {
	t.Helper()
	return SigningContext{
		GenesisHash:        testHash32(t, file.Genesis),
		SpecVersion:        file.SpecVersion,
		TransactionVersion: file.TransactionVersion,
		BirthBlockNumber:   vector.BirthBlock,
		BirthBlockHash:     testHash32(t, vector.BirthHash),
		Nonce:              vector.Nonce,
	}
}

type testSigner struct {
	public   [32]byte
	sig      [64]byte
	payloads [][]byte
}

func (self *testSigner) PublicKey() [32]byte {
	return self.public
}

func (self *testSigner) Sign(payload []byte) ([64]byte, error) {
	self.payloads = append(self.payloads, append([]byte(nil), payload...))
	return self.sig, nil
}

func TestFixtureIsRuntime486(t *testing.T) {
	meta := runtime486Metadata(t)
	if err := meta.checkVersion(RuntimeVersion{SpecName: "node-subtensor", SpecVersion: 486, TransactionVersion: 1}); err != nil {
		t.Fatal(err)
	}
	for _, other := range []RuntimeVersion{
		{SpecName: "node-subtensor", SpecVersion: 487, TransactionVersion: 1},
		{SpecName: "node-subtensor", SpecVersion: 486, TransactionVersion: 2},
		{SpecName: "other", SpecVersion: 486, TransactionVersion: 1},
	} {
		if err := meta.checkVersion(other); err == nil {
			t.Errorf("%+v passed for the metadata of 486", other)
		}
	}
	if meta.pallets["SubtensorModule"].index != 7 || meta.pallets["Commitments"].index != 18 {
		t.Fatal("pallet indices differ")
	}
	deposit, err := meta.ExistentialDeposit()
	if err != nil || deposit != 500 {
		t.Fatalf("existential deposit %d, %v", deposit, err)
	}
	minStake, err := meta.MinStakeRao()
	if err != nil || minStake != 2_000_000 {
		t.Fatalf("minimum stake %d, %v", minStake, err)
	}
}

func TestRuntime486SignedExtensions(t *testing.T) {
	meta := runtime486Metadata(t)
	identifiers := []string{}
	for _, extension := range meta.extensions {
		identifiers = append(identifiers, extension.identifier)
	}
	want := []string{
		"CheckNonZeroSender", "CheckSpecVersion", "CheckTxVersion", "CheckGenesis", "CheckMortality", "CheckNonce", "CheckWeight",
		"ChargeTransactionPayment", "SudoTransactionExtension", "CheckShieldedTxValidity", "SubtensorTransactionExtension", "DrandPriority",
		"CheckMetadataHash",
	}
	if !slices.Equal(identifiers, want) {
		t.Fatalf("extensions %v", identifiers)
	}
	plan, err := meta.signingPlan()
	if err != nil {
		t.Fatal(err)
	}
	wantPlan := []extensionStep{
		extensionEmpty, extensionSpecVersion, extensionTransactionVersion, extensionGenesis, extensionMortality, extensionNonce, extensionEmpty,
		extensionTip, extensionEmpty, extensionEmpty, extensionEmpty, extensionEmpty,
		extensionMetadataHash,
	}
	if !slices.Equal(plan, wantPlan) {
		t.Fatalf("plan %v", plan)
	}
}

// What the sdk builds is what another implementation builds from the same
// inputs: the call, the signing payload and, with that implementation's own
// signature in it, every byte of the extrinsic and its hash.
func TestVectorsFromAnotherImplementation(t *testing.T) {
	file := testVectors(t)
	meta := runtime486Metadata(t)
	for _, vector := range file.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			call := testVectorCall(t, vector)
			if vector.CallFunction != call.name {
				t.Fatalf("the vector is %s, the constructor %s", vector.CallFunction, call.name)
			}
			if got := hex.EncodeToString(call.encode()); got != vector.Call {
				t.Fatalf("call\n got %s\nwant %s", got, vector.Call)
			}
			signerKey := testHash32(t, vector.Signer)
			prepared, err := Prepare(meta, testSigningContext(t, file, vector), signerKey, call)
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(prepared.SigningPayload()); got != vector.Payload {
				t.Fatalf("payload\n got %s\nwant %s", got, vector.Payload)
			}
			signer := &testSigner{public: signerKey, sig: [64]byte(testUnhex(t, vector.Signature))}
			signed, err := prepared.Sign(signer)
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(signed.Raw); got != vector.Extrinsic {
				t.Fatalf("extrinsic\n got %s\nwant %s", got, vector.Extrinsic)
			}
			if got := hex.EncodeToString(signed.Hash[:]); got != vector.ExtrinsicHash {
				t.Fatalf("hash\n got %s\nwant %s", got, vector.ExtrinsicHash)
			}
			if len(signer.payloads) != 1 || hex.EncodeToString(signer.payloads[0]) != vector.Payload {
				t.Fatalf("the key signed %x", signer.payloads)
			}
		})
	}
}

func TestEncodeMortalEra(t *testing.T) {
	// the first four from py-scale-codec, the last two from real extrinsics on mainnet
	for _, row := range []struct {
		period, current uint64
		want            string
	}{
		{64, 9_422_016, "0500"},
		{64, 9_422_047, "f501"},
		{64, 7, "7500"},
		{64, 9_422_063, "f502"},
		{128, 9_421_415, "7606"},
		{128, 9_421_411, "3606"},
	} {
		got, err := EncodeMortalEra(row.period, row.current)
		if err != nil || hex.EncodeToString(got[:]) != row.want {
			t.Errorf("era %d at %d = %x, %v, want %s", row.period, row.current, got, err, row.want)
		}
	}
	for _, period := range []uint64{0, 1, 2, 3, 5, 63, 8192} {
		if _, err := EncodeMortalEra(period, 1); err == nil {
			t.Errorf("period %d passed", period)
		}
	}
}

func TestCompactEncoding(t *testing.T) {
	for _, row := range []struct {
		value uint64
		want  string
	}{
		{0, "00"},
		{1, "04"},
		{63, "fc"},
		{64, "0101"},
		{154, "6902"},
		{16383, "fdff"},
		{16384, "02000100"},
		{70_000, "c2450400"},
		{1<<30 - 1, "feffffff"},
		{1 << 30, "0300000040"},
		{1<<64 - 1, "13ffffffffffffffff"},
	} {
		encoded := appendCompact(nil, row.value)
		if hex.EncodeToString(encoded) != row.want {
			t.Errorf("%d = %x, want %s", row.value, encoded, row.want)
		}
		decoded, err := newScaleReader(encoded).readCompact()
		if err != nil || decoded != row.value {
			t.Errorf("%x read back %d, %v", encoded, decoded, err)
		}
	}
	if _, err := newScaleReader(testUnhex(t, "17ffffffffffffffffff")).readCompact(); err == nil {
		t.Error("a compact past 64 bits read as a uint64")
	}
	if _, err := newScaleReader(testUnhex(t, "13ffff")).readCompact(); err == nil {
		t.Error("a truncated compact read")
	}
}

func testRemoveStakeLimit(t *testing.T) *Call {
	t.Helper()
	call, err := NewRemoveStakeLimit([32]byte{1, 2, 3}, 25, 1_000, 2_000)
	if err != nil {
		t.Fatal(err)
	}
	return call
}

func testPrepare(meta *Metadata, call *Call) (*Prepared, error) {
	return Prepare(meta, SigningContext{GenesisHash: [32]byte{1}, SpecVersion: 486, TransactionVersion: 1, BirthBlockNumber: 100, BirthBlockHash: [32]byte{2}, Nonce: 3}, [32]byte{4}, call)
}

// A runtime this client does not know refuses before anything can be signed.
func TestAChangedRuntimeRefuses(t *testing.T) {
	palletCalls := func(meta *Metadata, pallet string) *metadataType {
		return meta.types[meta.pallets[pallet].calls]
	}
	removeStakeLimit := func(meta *Metadata) *metadataVariant {
		return palletCalls(meta, "SubtensorModule").variantByName("remove_stake_limit")
	}
	extensionNamed := func(meta *Metadata, name string) int {
		for i, extension := range meta.extensions {
			if extension.identifier == name {
				return i
			}
		}
		t.Fatalf("no extension %s", name)
		return 0
	}
	// a type id no field of the fixture uses, with the given definition
	addType := func(meta *Metadata, ty *metadataType) uint32 {
		id := uint32(len(meta.types) + 1000)
		meta.types[id] = ty
		return id
	}
	u128Type := func(meta *Metadata) uint32 {
		for id, ty := range meta.types {
			if ty.kind == typeDefPrimitive && ty.primitive == primitiveU128 {
				return id
			}
		}
		t.Fatal("no u128 in the fixture")
		return 0
	}
	for _, row := range []struct {
		name   string
		change func(*Metadata)
		want   string
	}{
		{"the extrinsic version", func(meta *Metadata) { meta.extrinsicVersion = 5 }, "extrinsic version"},
		{"the pallet moves", func(meta *Metadata) { meta.pallets["SubtensorModule"].index = 8 }, "index 8"},
		{"the pallet is gone", func(meta *Metadata) { delete(meta.pallets, "SubtensorModule") }, "gone"},
		{"the call moves", func(meta *Metadata) { removeStakeLimit(meta).index = 90 }, "index 90"},
		{"the call is gone", func(meta *Metadata) { removeStakeLimit(meta).name = "remove_stake_limit_v2" }, "gone"},
		{"a new argument", func(meta *Metadata) {
			variant := removeStakeLimit(meta)
			variant.fields = append(variant.fields, metadataField{name: "memo", ty: variant.fields[0].ty})
		}, "6 arguments"},
		{"an argument goes", func(meta *Metadata) {
			variant := removeStakeLimit(meta)
			variant.fields = variant.fields[:4]
		}, "4 arguments"},
		{"an argument renamed", func(meta *Metadata) { removeStakeLimit(meta).fields[3].name = "limit" }, "limit"},
		{"arguments swapped", func(meta *Metadata) {
			fields := removeStakeLimit(meta).fields
			fields[2], fields[3] = fields[3], fields[2]
		}, "argument 2"},
		{"the amount is wider", func(meta *Metadata) { removeStakeLimit(meta).fields[2].ty = u128Type(meta) }, "amount_unstaked"},
		{"the hotkey is not an account", func(meta *Metadata) { removeStakeLimit(meta).fields[0].ty = removeStakeLimit(meta).fields[2].ty }, "hotkey"},
		{"a new extension with bytes in the transaction", func(meta *Metadata) {
			meta.extensions = append(meta.extensions, metadataExtension{identifier: "NewExtension", ty: meta.extensions[extensionNamed(meta, "CheckNonce")].ty, additional: meta.extensions[extensionNamed(meta, "CheckWeight")].additional})
		}, "NewExtension"},
		{"a new extension with bytes in the payload", func(meta *Metadata) {
			meta.extensions = append(meta.extensions, metadataExtension{identifier: "NewExtension", ty: meta.extensions[extensionNamed(meta, "CheckWeight")].ty, additional: meta.extensions[extensionNamed(meta, "CheckSpecVersion")].additional})
		}, "NewExtension"},
		{"the nonce goes", func(meta *Metadata) {
			meta.extensions = slices.Delete(meta.extensions, extensionNamed(meta, "CheckNonce"), extensionNamed(meta, "CheckNonce")+1)
		}, "CheckNonce"},
		{"the era goes", func(meta *Metadata) {
			meta.extensions = slices.Delete(meta.extensions, extensionNamed(meta, "CheckMortality"), extensionNamed(meta, "CheckMortality")+1)
		}, "CheckMortality"},
		{"the genesis goes", func(meta *Metadata) {
			meta.extensions = slices.Delete(meta.extensions, extensionNamed(meta, "CheckGenesis"), extensionNamed(meta, "CheckGenesis")+1)
		}, "CheckGenesis"},
		{"the nonce twice", func(meta *Metadata) {
			meta.extensions = append(meta.extensions, meta.extensions[extensionNamed(meta, "CheckNonce")])
		}, "CheckNonce"},
		{"the nonce is not compact", func(meta *Metadata) {
			meta.extensions[extensionNamed(meta, "CheckNonce")].ty = meta.extensions[extensionNamed(meta, "CheckSpecVersion")].additional
		}, "CheckNonce"},
		{"the tip's balance is wider", func(meta *Metadata) {
			meta.extensions[extensionNamed(meta, "ChargeTransactionPayment")].ty = addType(meta, &metadataType{kind: typeDefCompact, elem: u128Type(meta)})
		}, "ChargeTransactionPayment"},
		{"the era's block is not a hash", func(meta *Metadata) {
			meta.extensions[extensionNamed(meta, "CheckMortality")].additional = meta.extensions[extensionNamed(meta, "CheckSpecVersion")].additional
		}, "CheckMortality"},
		{"the metadata hash's disabled mode moves", func(meta *Metadata) {
			mode, _ := meta.resolve(meta.extensions[extensionNamed(meta, "CheckMetadataHash")].ty)
			mode.variantByName("Disabled").index = 7
		}, "CheckMetadataHash"},
		{"the signature's sr25519 moves", func(meta *Metadata) {
			extrinsic := meta.types[meta.extrinsicType]
			signatureId, _ := extrinsic.param("Signature")
			signature, _ := meta.resolve(signatureId)
			signature.variantByName("Sr25519").index = 9
		}, "Sr25519"},
		{"the address's id moves", func(meta *Metadata) {
			extrinsic := meta.types[meta.extrinsicType]
			addressId, _ := extrinsic.param("Address")
			address, _ := meta.resolve(addressId)
			address.variantByName("Id").index = 9
		}, "address"},
	} {
		t.Run(row.name, func(t *testing.T) {
			meta := runtime486Metadata(t)
			if _, err := testPrepare(meta, testRemoveStakeLimit(t)); err != nil {
				t.Fatalf("CONTROL FAILED: the unchanged runtime refused: %v", err)
			}
			row.change(meta)
			prepared, err := testPrepare(meta, testRemoveStakeLimit(t))
			if err == nil || prepared != nil {
				t.Fatal("prepared under a changed runtime")
			}
			if !IsRuntimeChanged(err) {
				t.Fatalf("refused with another error: %v", err)
			}
			if !strings.Contains(err.Error(), row.want) {
				t.Fatalf("the refusal %q does not name %q", err, row.want)
			}
		})
	}
}

// A runtime upgrade that adds an extension with no bytes changes nothing to
// sign, so it needs no release.
func TestAnEmptyNewExtensionStillSigns(t *testing.T) {
	meta := runtime486Metadata(t)
	before, err := testPrepare(meta, testRemoveStakeLimit(t))
	if err != nil {
		t.Fatal(err)
	}
	var weight metadataExtension
	for _, extension := range meta.extensions {
		if extension.identifier == "CheckWeight" {
			weight = extension
		}
	}
	meta.extensions = append(meta.extensions[:3:3], append([]metadataExtension{{identifier: "SomethingNew", ty: weight.ty, additional: weight.additional}}, meta.extensions[3:]...)...)
	after, err := testPrepare(meta, testRemoveStakeLimit(t))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.SigningPayload(), after.SigningPayload()) {
		t.Fatal("an empty extension changed the payload")
	}
}

// A call the metadata does not have at its pinned place refuses, whatever else
// the metadata says: altered metadata cannot move a call's meaning.
func TestAlteredMetadataCannotMoveACallOntoAnother(t *testing.T) {
	meta := runtime486Metadata(t)
	// Metadata that serves Balances where SubtensorModule is, say: with
	// remove_stake_limit at Balances' index and the transfer's call index.
	balances := meta.pallets["Balances"]
	subtensor := meta.pallets["SubtensorModule"]
	subtensor.index, balances.index = balances.index, subtensor.index
	if _, err := testPrepare(meta, testRemoveStakeLimit(t)); !IsRuntimeChanged(err) {
		t.Fatalf("prepared with the pallet at another index: %v", err)
	}
}

func TestConstructorsRefuseMissingArguments(t *testing.T) {
	hotkey := [32]byte{1}
	for name, err := range map[string]error{
		"remove_stake_limit hotkey": func() error { _, err := NewRemoveStakeLimit([32]byte{}, 25, 1, 1); return err }(),
		"remove_stake_limit netuid": func() error { _, err := NewRemoveStakeLimit(hotkey, 0, 1, 1); return err }(),
		"remove_stake_limit amount": func() error { _, err := NewRemoveStakeLimit(hotkey, 25, 0, 1); return err }(),
		"remove_stake_limit limit":  func() error { _, err := NewRemoveStakeLimit(hotkey, 25, 1, 0); return err }(),
		"register_limit hotkey":     func() error { _, err := NewRegisterLimit(25, [32]byte{}, 1); return err }(),
		"register_limit netuid":     func() error { _, err := NewRegisterLimit(0, hotkey, 1); return err }(),
		"register_limit limit":      func() error { _, err := NewRegisterLimit(25, hotkey, 0); return err }(),
		"set_commitment netuid":     func() error { _, err := NewSetFleetCommitment(0, [32]byte{1}); return err }(),
		"set_commitment hash":       func() error { _, err := NewSetFleetCommitment(25, [32]byte{}); return err }(),
	} {
		if err == nil {
			t.Errorf("%s: no refusal", name)
		}
	}
}

// Whatever the typed arguments, the bytes are that one call and no other: the
// pinned indices, fill-or-kill, and arguments at fixed places.
func TestArgumentsOnlyEverFillTheFixedCall(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	meta := runtime486Metadata(t)
	for range 2000 {
		var hotkey [32]byte
		binary.LittleEndian.PutUint64(hotkey[:], random.Uint64()|1)
		binary.LittleEndian.PutUint64(hotkey[24:], random.Uint64())
		netuid := uint16(random.UintN(65535)) + 1
		amount := random.Uint64() | 1
		limit := random.Uint64() | 1
		call, err := NewRemoveStakeLimit(hotkey, netuid, amount, limit)
		if err != nil {
			t.Fatal(err)
		}
		encoded := call.encode()
		want := []byte{7, 89}
		want = append(want, hotkey[:]...)
		want = binary.LittleEndian.AppendUint16(want, netuid)
		want = binary.LittleEndian.AppendUint64(want, amount)
		want = binary.LittleEndian.AppendUint64(want, limit)
		want = append(want, 0)
		if !bytes.Equal(encoded, want) {
			t.Fatalf("got %x, want %x", encoded, want)
		}
		if err := meta.checkCall(call); err != nil {
			t.Fatal(err)
		}
	}
}

// The only way to get a Call is a constructor in calls.go, and nothing takes a
// payload or call bytes to sign. New calls have to be added here on purpose.
func TestOnlyTheFixedCallsCanBeMade(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	constructors := []string{}
	exported := []string{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if name, ok := literal.Type.(*ast.Ident); ok && name.Name == "Call" {
				position := fileSet.Position(literal.Pos())
				if filepath.Base(position.Filename) != "calls.go" {
					t.Errorf("a Call is made at %s", position)
				}
			}
			return true
		})
		for _, declaration := range file.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				if !declaration.Name.IsExported() {
					continue
				}
				exported = append(exported, declaration.Name.Name)
				if declaration.Recv == nil && declaration.Type.Results != nil {
					for _, result := range declaration.Type.Results.List {
						star, ok := result.Type.(*ast.StarExpr)
						if !ok {
							continue
						}
						if name, ok := star.X.(*ast.Ident); ok && name.Name == "Call" {
							constructors = append(constructors, declaration.Name.Name)
						}
					}
				}
				// no exported function takes raw bytes but the two that read
				// what a chain sent: metadata and a transaction to quote
				if declaration.Type.Params != nil {
					for _, param := range declaration.Type.Params.List {
						array, ok := param.Type.(*ast.ArrayType)
						if !ok || array.Len != nil {
							continue
						}
						if declaration.Name.Name != "ParseMetadata" && declaration.Name.Name != "QuoteFee" {
							t.Errorf("%s takes a slice", declaration.Name.Name)
						}
					}
				}
			case *ast.GenDecl:
				for _, spec := range declaration.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok || typeSpec.Name.Name != "Call" {
						continue
					}
					structType, ok := typeSpec.Type.(*ast.StructType)
					if !ok {
						t.Fatal("Call is not a struct")
					}
					for _, field := range structType.Fields.List {
						for _, name := range field.Names {
							if name.IsExported() {
								t.Errorf("Call.%s can be set from outside", name.Name)
							}
						}
					}
				}
			}
		}
	}
	slices.Sort(constructors)
	want := []string{"NewRegisterLimit", "NewRemoveStakeLimit", "NewSetFleetCommitment"}
	if !slices.Equal(constructors, want) {
		t.Fatalf("the constructors of a Call are %v, want %v", constructors, want)
	}
	if !slices.Contains(exported, "NewRemoveStakeLimit") || !slices.Contains(exported, "Prepare") {
		t.Fatalf("CONTROL FAILED: the scan saw %v", exported)
	}
}

func TestALongPayloadIsSignedByItsHash(t *testing.T) {
	long := &Prepared{signer: [32]byte{1}, call: make([]byte, 200), extra: make([]byte, 50), additional: make([]byte, 7)}
	sum := blake2b.Sum256(make([]byte, 257))
	if !bytes.Equal(long.SigningPayload(), sum[:]) {
		t.Fatal("a 257-byte payload was not hashed")
	}
	exact := &Prepared{signer: [32]byte{1}, call: make([]byte, 200), extra: make([]byte, 50), additional: make([]byte, 6)}
	if !bytes.Equal(exact.SigningPayload(), make([]byte, 256)) {
		t.Fatal("a 256-byte payload was hashed")
	}
}

func TestSignRefusesAnotherKey(t *testing.T) {
	meta := runtime486Metadata(t)
	prepared, err := testPrepare(meta, testRemoveStakeLimit(t))
	if err != nil {
		t.Fatal(err)
	}
	signer := &testSigner{public: [32]byte{9}}
	if _, err := prepared.Sign(signer); err == nil {
		t.Fatal("signed for another key")
	}
	if len(signer.payloads) != 0 {
		t.Fatal("the other key was asked to sign")
	}
	if _, err := prepared.Sign(nil); err == nil {
		t.Fatal("signed with no key")
	}
}

// An endpoint chooses these bytes. Damage has to come back as an error.
func TestDamagedMetadataIsAnError(t *testing.T) {
	raw := runtime486MetadataBytes(t)
	random := rand.New(rand.NewPCG(3, 4))
	for range 60 {
		if _, err := ParseMetadata(raw[:random.IntN(len(raw))]); err == nil {
			t.Fatal("truncated metadata parsed")
		}
	}
	for range 150 {
		damaged := slices.Clone(raw)
		for range 1 + random.IntN(8) {
			damaged[random.IntN(len(damaged))] ^= byte(1 << random.IntN(8))
		}
		// parsing may pass, when the damage lands in a name; it may not panic
		meta, err := ParseMetadata(damaged)
		if err == nil {
			if _, err := testPrepare(meta, testRemoveStakeLimit(t)); err != nil && !IsRuntimeChanged(err) {
				t.Fatalf("not a refusal: %v", err)
			}
		}
	}
	if _, err := ParseMetadata(append(slices.Clone(raw), 0)); err == nil {
		t.Fatal("trailing bytes parsed")
	}
	if _, err := ParseMetadata(nil); err == nil {
		t.Fatal("no bytes parsed")
	}
	other := slices.Clone(raw)
	other[4] = 15
	if _, err := ParseMetadata(other); err == nil {
		t.Fatal("another metadata version parsed")
	}
}

// Cycles and fan-out in a type registry are an endpoint's to send.
func TestPathologicalTypesAreBounded(t *testing.T) {
	meta := &Metadata{types: map[uint32]*metadataType{
		0: {kind: typeDefComposite, fields: []metadataField{{ty: 0}}},
		1: {kind: typeDefComposite, fields: []metadataField{{ty: 1}, {ty: 1}}},
		2: {kind: typeDefPrimitive, primitive: primitiveU8},
	}}
	// a fan-out of depth 40 that a walk without a limit would never finish
	for i := uint32(3); i < 43; i++ {
		meta.types[i] = &metadataType{kind: typeDefComposite, fields: []metadataField{{ty: i - 1}, {ty: i - 1}}}
	}
	done := make(chan error, 3)
	go func() {
		_, err := meta.decodeValue(make([]byte, 64), 0)
		done <- err
	}()
	go func() {
		_, err := meta.isZeroSized(1)
		done <- err
	}()
	go func() {
		_, err := meta.decodeValue(make([]byte, 1<<12), 42)
		done <- err
	}()
	for range 3 {
		select {
		case err := <-done:
			if err == nil {
				t.Error("a pathological type decoded")
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a pathological type did not finish")
		}
	}
	if _, err := meta.resolve(0); err == nil {
		t.Error("a cycle resolved")
	}
}

func TestStakeInfoParsing(t *testing.T) {
	hotkey, coldkey := [32]byte{1}, [32]byte{2}
	good := func(netuid uint64, stake uint64) []byte {
		out := []byte{1}
		out = append(out, hotkey[:]...)
		out = append(out, coldkey[:]...)
		out = appendCompact(out, netuid)
		out = appendCompact(out, stake)
		out = appendCompact(out, 0)
		out = appendCompact(out, 7)
		out = appendCompact(out, 0)
		out = appendCompact(out, 0)
		return append(out, 1)
	}
	stake, err := parseStakeInfo(good(25, 123_456_789_012), hotkey, coldkey, 25)
	if err != nil || stake != 123_456_789_012 {
		t.Fatalf("stake %d, %v", stake, err)
	}
	if stake, err := parseStakeInfo([]byte{0}, hotkey, coldkey, 25); err != nil || stake != 0 {
		t.Fatalf("no info: %d, %v", stake, err)
	}
	for name, reply := range map[string][]byte{
		"empty":         {},
		"bad option":    {2},
		"none and more": {0, 0},
		"trailing":      append(good(25, 1), 0),
		"short":         good(25, 1)[:60],
		"other subnet":  good(26, 1),
	} {
		if _, err := parseStakeInfo(reply, hotkey, coldkey, 25); !IsRuntimeChanged(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := parseStakeInfo(good(25, 1), [32]byte{3}, coldkey, 25); !IsRuntimeChanged(err) {
		t.Errorf("another hotkey: %v", err)
	}
	if _, err := parseStakeInfo(good(25, 1), hotkey, [32]byte{3}, 25); !IsRuntimeChanged(err) {
		t.Errorf("another coldkey: %v", err)
	}
}

func TestFeeQuoteParsing(t *testing.T) {
	reply := appendCompact(nil, 3_000_000_000)
	reply = appendCompact(reply, 70_000)
	reply = append(reply, 0)
	reply = binary.LittleEndian.AppendUint64(reply, 629_362)
	fee, err := parseFeeQuote(reply)
	if err != nil || fee != 629_362 {
		t.Fatalf("fee %d, %v", fee, err)
	}
	if _, err := parseFeeQuote(reply[:len(reply)-1]); !IsRuntimeChanged(err) {
		t.Fatalf("a short fee: %v", err)
	}
	if _, err := parseFeeQuote(append(slices.Clone(reply), 0)); !IsRuntimeChanged(err) {
		t.Fatalf("a long fee: %v", err)
	}
}

// One hash, by the definition: twox128 is two xxh64 runs, seeds 0 and 1, little
// endian. Enough of xxh64 for keys under 32 bytes.
func testXxh64(data []byte, seed uint64) uint64 {
	const (
		prime1 uint64 = 11400714785074694791
		prime2 uint64 = 14029467366897019727
		prime3 uint64 = 1609587929392839161
		prime4 uint64 = 9650029242287828579
		prime5 uint64 = 2870177450012600261
	)
	hash := seed + prime5 + uint64(len(data))
	for ; len(data) >= 8; data = data[8:] {
		k := bits.RotateLeft64(binary.LittleEndian.Uint64(data)*prime2, 31) * prime1
		hash = bits.RotateLeft64(hash^k, 27)*prime1 + prime4
	}
	if len(data) >= 4 {
		hash = bits.RotateLeft64(hash^uint64(binary.LittleEndian.Uint32(data))*prime1, 23)*prime2 + prime3
		data = data[4:]
	}
	for _, b := range data {
		hash = bits.RotateLeft64(hash^uint64(b)*prime5, 11) * prime1
	}
	hash ^= hash >> 33
	hash *= prime2
	hash ^= hash >> 29
	hash *= prime3
	hash ^= hash >> 32
	return hash
}

func testTwox128(name string) string {
	out := binary.LittleEndian.AppendUint64(nil, testXxh64([]byte(name), 0))
	out = binary.LittleEndian.AppendUint64(out, testXxh64([]byte(name), 1))
	return hex.EncodeToString(out)
}

func TestStorageKeys(t *testing.T) {
	// a well-known key, and so a check of the hash itself
	if got := testTwox128("System"); got != "26aa394eea5630e07c48ae0c9558cef7" {
		t.Fatalf("twox128(System) = %s", got)
	}
	for _, row := range []struct {
		pinned string
		pallet string
		item   string
	}{
		{storageSystemAccount, "System", "Account"},
		{storageSystemEvents, "System", "Events"},
		{storageSubtensorBurn, "SubtensorModule", "Burn"},
		{storageSubtensorNominatorMinimum, "SubtensorModule", "NominatorMinRequiredStake"},
	} {
		if want := testTwox128(row.pallet) + testTwox128(row.item); row.pinned != want {
			t.Errorf("%s.%s is %s, want %s", row.pallet, row.item, row.pinned, want)
		}
	}
	account := [32]byte(testUnhex(t, "d43593c715fdd31c61141abd04a99fd6822c8558854ccde39a5684e7a56da27d"))
	// //Alice's key as substrate-interface computes it
	want := "0x26aa394eea5630e07c48ae0c9558cef7b99d880ec681799c0cf30e8886371da9de1e86a9a8c739864cf3cc5ec2bea59fd43593c715fdd31c61141abd04a99fd6822c8558854ccde39a5684e7a56da27d"
	if got := accountStorageKey(account); got != want {
		t.Fatalf("account key\n got %s\nwant %s", got, want)
	}
}

// A fake endpoint: canned answers by method and first argument.
type fakeRpc struct {
	answers map[string]func(params []any) (any, error)
	calls   []string
}

func (self *fakeRpc) Call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	self.calls = append(self.calls, method)
	answer, ok := self.answers[method]
	if !ok {
		return nil, fmt.Errorf("no answer for %s", method)
	}
	value, err := answer(params)
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func (self *fakeRpc) Batch(ctx context.Context, calls []evm.BatchCall) ([]evm.BatchResult, error) {
	results := make([]evm.BatchResult, len(calls))
	for i, call := range calls {
		results[i].Result, results[i].Err = self.Call(ctx, call.Method, call.Params...)
	}
	return results, nil
}

// The events of one block: a list of event records.
type testEvents struct {
	count uint64
	body  []byte
}

func (self *testEvents) add(extrinsic uint32, pallet byte, event byte, fields []byte) {
	self.count += 1
	self.body = append(self.body, 0)
	self.body = binary.LittleEndian.AppendUint32(self.body, extrinsic)
	self.body = append(self.body, pallet, event)
	self.body = append(self.body, fields...)
	self.body = append(self.body, 0)
}

func (self *testEvents) encode() string {
	return hexPrefixed(append(appendCompact(nil, self.count), self.body...))
}

func testDispatchInfo() []byte {
	out := appendCompact(nil, 1_000)
	out = appendCompact(out, 1_000)
	return append(out, 0, 0)
}

func testStakeRemoved(coldkey [32]byte, hotkey [32]byte, tao uint64, alpha uint64, netuid uint16, poolFee uint64) []byte {
	out := append([]byte(nil), coldkey[:]...)
	out = append(out, hotkey[:]...)
	out = binary.LittleEndian.AppendUint64(out, tao)
	out = binary.LittleEndian.AppendUint64(out, alpha)
	out = binary.LittleEndian.AppendUint16(out, netuid)
	return binary.LittleEndian.AppendUint64(out, poolFee)
}

func testBlockChain(t *testing.T, head uint64, landing map[uint64][]string, events map[uint64]string) *fakeRpc {
	t.Helper()
	blockHash := func(number uint64) [32]byte {
		var hash [32]byte
		binary.BigEndian.PutUint64(hash[24:], number)
		hash[0] = 0xb1
		return hash
	}
	numberOf := func(value any) uint64 {
		data, err := parseHexBytes(value.(string))
		if err != nil || len(data) != 32 {
			t.Fatalf("bad hash %v", value)
		}
		return binary.BigEndian.Uint64(data[24:])
	}
	return &fakeRpc{answers: map[string]func(params []any) (any, error){
		"chain_getFinalizedHead": func(params []any) (any, error) {
			hash := blockHash(head)
			return hexPrefixed(hash[:]), nil
		},
		"chain_getHeader": func(params []any) (any, error) {
			return map[string]string{"number": fmt.Sprintf("0x%x", numberOf(params[0]))}, nil
		},
		"chain_getBlockHash": func(params []any) (any, error) {
			hash := blockHash(params[0].(uint64))
			return hexPrefixed(hash[:]), nil
		},
		"chain_getBlock": func(params []any) (any, error) {
			body := map[string]any{"extrinsics": landing[numberOf(params[0])]}
			return map[string]any{"block": body}, nil
		},
		"state_getStorage": func(params []any) (any, error) {
			return events[numberOf(params[1])], nil
		},
	}}
}

func testNoSleep(ctx context.Context, wait time.Duration) error {
	return ctx.Err()
}

func TestWaitOutcomeReadsTheFinalizedResult(t *testing.T) {
	meta := runtime486Metadata(t)
	coldkey, hotkey := [32]byte{5}, [32]byte{6}
	raw := []byte{1, 2, 3, 4}
	hash := blake2b.Sum256(raw)
	events := &testEvents{}
	// another transaction in the same block, not ours
	events.add(0, 0, 0, testDispatchInfo())
	feeSale := testStakeRemoved(coldkey, hotkey, 652_113, 150_000_000, 25, 0)
	events.add(1, 7, 3, feeSale)
	events.add(1, 7, 3, testStakeRemoved(coldkey, hotkey, 4_100_000, 999_000_000, 25, 500_000))
	feePaid := append(append([]byte(nil), coldkey[:]...), 25, 0)
	feePaid = binary.LittleEndian.AppendUint64(feePaid, 150_000_000)
	feePaid = binary.LittleEndian.AppendUint64(feePaid, 640_000)
	events.add(1, 7, 128, feePaid)
	events.add(1, 0, 0, testDispatchInfo())
	rpc := testBlockChain(t, 112, map[uint64][]string{
		110: {hexPrefixed([]byte{9, 9})},
		111: {hexPrefixed([]byte{9, 9}), hexPrefixed(raw)},
	}, map[uint64]string{111: events.encode()})
	outcome, err := NewChain(rpc).WaitOutcome(context.Background(), meta, hash, WaitOptions{FirstBlock: 101, LastBlock: 164, Poll: time.Second, Sleep: testNoSleep})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Success || outcome.Failure != "" || outcome.Block.Number != 111 || outcome.ExtrinsicIndex != 1 {
		t.Fatalf("outcome %+v", outcome)
	}
	if len(outcome.Sales) != 2 || outcome.Sales[1].TaoRao != 4_100_000 || outcome.Sales[1].AlphaRao != 999_000_000 || outcome.Sales[1].PoolFeeAlphaRao != 500_000 || outcome.Sales[1].Coldkey != coldkey || outcome.Sales[1].Hotkey != hotkey || outcome.Sales[1].Netuid != 25 {
		t.Fatalf("sales %+v", outcome.Sales)
	}
	if outcome.AlphaFee == nil || outcome.AlphaFee.AlphaRao != 150_000_000 || outcome.AlphaFee.TaoRao != 640_000 {
		t.Fatalf("alpha fee %+v", outcome.AlphaFee)
	}
}

func TestWaitOutcomeNamesAFailedCall(t *testing.T) {
	meta := runtime486Metadata(t)
	raw := []byte{1, 2, 3, 4}
	hash := blake2b.Sum256(raw)
	for _, row := range []struct {
		name    string
		failure []byte
		want    string
	}{
		{"slippage", []byte{3, 7, 84, 0, 0, 0}, "SubtensorModule.SlippageTooHigh"},
		{"not enough stake", []byte{3, 7, 9, 0, 0, 0}, "SubtensorModule.NotEnoughStakeToWithdraw"},
		{"the swap's limit", []byte{3, 28, 3, 0, 0, 0}, "Swap.PriceLimitExceeded"},
		{"a bad origin", []byte{2}, "BadOrigin"},
		{"an unknown pallet", []byte{3, 200, 0, 0, 0, 0}, "Module 200"},
	} {
		t.Run(row.name, func(t *testing.T) {
			events := &testEvents{}
			events.add(0, 0, 1, append(slices.Clone(row.failure), testDispatchInfo()...))
			rpc := testBlockChain(t, 101, map[uint64][]string{101: {hexPrefixed(raw)}}, map[uint64]string{101: events.encode()})
			outcome, err := NewChain(rpc).WaitOutcome(context.Background(), meta, hash, WaitOptions{FirstBlock: 101, LastBlock: 164, Poll: time.Second, Sleep: testNoSleep})
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Success || outcome.Failure != row.want {
				t.Fatalf("outcome %+v", outcome)
			}
		})
	}
}

func TestWaitOutcomeEndsWithTheEra(t *testing.T) {
	meta := runtime486Metadata(t)
	rpc := testBlockChain(t, 170, map[uint64][]string{}, map[uint64]string{})
	_, err := NewChain(rpc).WaitOutcome(context.Background(), meta, [32]byte{1}, WaitOptions{FirstBlock: 101, LastBlock: 164, Poll: time.Second, Sleep: testNoSleep})
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("after the era: %v", err)
	}
}

func TestWaitOutcomeKeepsWaitingWhileTheEraIsOpen(t *testing.T) {
	meta := runtime486Metadata(t)
	raw := []byte{1, 2, 3, 4}
	hash := blake2b.Sum256(raw)
	events := &testEvents{}
	events.add(0, 0, 0, testDispatchInfo())
	head := uint64(100)
	rpc := testBlockChain(t, 164, map[uint64][]string{164: {hexPrefixed(raw)}}, map[uint64]string{164: events.encode()})
	// the head moves when waited for, the endpoint fails once on the way
	realHead := rpc.answers["chain_getFinalizedHead"]
	failed := false
	rpc.answers["chain_getFinalizedHead"] = func(params []any) (any, error) {
		if !failed && head == 130 {
			failed = true
			return nil, errors.New("the endpoint is busy")
		}
		var hash [32]byte
		binary.BigEndian.PutUint64(hash[24:], head)
		_ = realHead
		return hexPrefixed(hash[:]), nil
	}
	sleeps := 0
	outcome, err := NewChain(rpc).WaitOutcome(context.Background(), meta, hash, WaitOptions{FirstBlock: 101, LastBlock: 164, Poll: time.Second, Sleep: func(ctx context.Context, wait time.Duration) error {
		sleeps += 1
		head += 10
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Success || outcome.Block.Number != 164 {
		t.Fatalf("outcome %+v", outcome)
	}
	if !failed || sleeps < 6 {
		t.Fatalf("CONTROL FAILED: failed %v, slept %d", failed, sleeps)
	}
}

func TestWaitOutcomeSaysWhenItLandedAndCannotBeRead(t *testing.T) {
	meta := runtime486Metadata(t)
	raw := []byte{1, 2, 3, 4}
	hash := blake2b.Sum256(raw)
	for name, events := range map[string]string{
		"events that do not decode": "0x0401",
		"events without a result": func() string {
			events := &testEvents{}
			events.add(5, 0, 0, testDispatchInfo())
			return events.encode()
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			rpc := testBlockChain(t, 102, map[uint64][]string{102: {hexPrefixed(raw)}}, map[uint64]string{102: events})
			_, err := NewChain(rpc).WaitOutcome(context.Background(), meta, hash, WaitOptions{FirstBlock: 101, LastBlock: 164, Poll: time.Second, Sleep: func(ctx context.Context, wait time.Duration) error {
				t.Error("it waited for an answer that will not change")
				return errors.New("stop")
			}})
			if !errors.Is(err, ErrOutcomeUnreadable) {
				t.Fatalf("unreadable events: %v", err)
			}
		})
	}
}

func TestReadSaleState(t *testing.T) {
	meta := runtime486Metadata(t)
	hotkey, coldkey := [32]byte{1}, [32]byte{2}
	stake := []byte{1}
	stake = append(stake, hotkey[:]...)
	stake = append(stake, coldkey[:]...)
	stake = appendCompact(stake, 25)
	stake = appendCompact(stake, 5_000_000_000)
	for range 4 {
		stake = appendCompact(stake, 0)
	}
	stake = append(stake, 0)
	rpc := &fakeRpc{answers: map[string]func(params []any) (any, error){
		"state_call": func(params []any) (any, error) {
			switch params[0] {
			case "StakeInfoRuntimeApi_get_stake_info_for_hotkey_coldkey_netuid":
				if params[1] != stakeInfoHex(hotkey, coldkey, 25) {
					t.Errorf("stake args %v", params[1])
				}
				return hexPrefixed(stake), nil
			case "SwapRuntimeApi_current_alpha_price":
				if params[1] != "0x1900" {
					t.Errorf("price args %v", params[1])
				}
				return hexPrefixed(binary.LittleEndian.AppendUint64(nil, 4_153_679)), nil
			}
			return nil, fmt.Errorf("no answer for %v", params[0])
		},
		"state_getStorage": func(params []any) (any, error) {
			switch params[0] {
			case accountStorageKey(coldkey):
				// an account with no entry: a key that never held TAO
				return nil, nil
			case storageKey(storageSubtensorNominatorMinimum, nil):
				return hexPrefixed(binary.LittleEndian.AppendUint64(nil, 10_000_000)), nil
			}
			return nil, fmt.Errorf("no answer for %v", params[0])
		},
	}}
	state, err := NewChain(rpc).ReadSaleState(context.Background(), meta, hotkey, coldkey, 25)
	if err != nil {
		t.Fatal(err)
	}
	// 2,000,000 rao of minimum stake times 10,000,000 millionths, as the runtime works it out
	if state.StakeRao != 5_000_000_000 || state.Balance != (AccountBalance{}) || state.PriceRao != 4_153_679 || state.NominatorMinimumRao != 20_000_000 {
		t.Fatalf("state %+v", state)
	}
}

func stakeInfoHex(hotkey [32]byte, coldkey [32]byte, netuid uint16) string {
	return hexPrefixed(stakeInfoArgs(hotkey, coldkey, netuid))
}

func TestAccountBalanceDecodesTheRuntimesLayout(t *testing.T) {
	meta := runtime486Metadata(t)
	account := []byte{}
	account = binary.LittleEndian.AppendUint32(account, 3)
	account = binary.LittleEndian.AppendUint32(account, 0)
	account = binary.LittleEndian.AppendUint32(account, 1)
	account = binary.LittleEndian.AppendUint32(account, 1)
	account = binary.LittleEndian.AppendUint64(account, 123_456)
	account = binary.LittleEndian.AppendUint64(account, 7)
	account = binary.LittleEndian.AppendUint64(account, 9)
	account = append(account, make([]byte, 16)...)
	rpc := &fakeRpc{answers: map[string]func(params []any) (any, error){
		"state_getStorage": func(params []any) (any, error) {
			return hexPrefixed(account), nil
		},
	}}
	balance, err := NewChain(rpc).ReadAccountBalance(context.Background(), meta, [32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if balance != (AccountBalance{FreeRao: 123_456, ReservedRao: 7, FrozenRao: 9, Providers: 1, Sufficients: 1}) {
		t.Fatalf("balance %+v", balance)
	}
	rpc.answers["state_getStorage"] = func(params []any) (any, error) {
		return hexPrefixed(account[:40]), nil
	}
	if _, err := NewChain(rpc).ReadAccountBalance(context.Background(), meta, [32]byte{1}); !IsRuntimeChanged(err) {
		t.Fatalf("a short account: %v", err)
	}
}

func TestSubmitChecksTheNodesHash(t *testing.T) {
	raw := []byte{1, 2, 3, 4}
	signed := &Signed{Raw: raw, Hash: blake2b.Sum256(raw)}
	rpc := &fakeRpc{answers: map[string]func(params []any) (any, error){
		"author_submitExtrinsic": func(params []any) (any, error) {
			if params[0] != "0x01020304" {
				t.Errorf("sent %v", params[0])
			}
			return hexPrefixed(signed.Hash[:]), nil
		},
	}}
	if err := NewChain(rpc).Submit(context.Background(), signed); err != nil {
		t.Fatal(err)
	}
	rpc.answers["author_submitExtrinsic"] = func(params []any) (any, error) {
		return hexPrefixed(make([]byte, 32)), nil
	}
	if err := NewChain(rpc).Submit(context.Background(), signed); err == nil {
		t.Fatal("another hash passed")
	}
	rpc.answers["author_submitExtrinsic"] = func(params []any) (any, error) {
		return nil, &evm.RpcError{Code: 1010, Message: "Invalid Transaction", Data: json.RawMessage(`"Transaction has a bad signature"`)}
	}
	var refusal *SubmitRefusal
	if err := NewChain(rpc).Submit(context.Background(), signed); !errors.As(err, &refusal) || refusal.Code != 1010 || !strings.Contains(refusal.Data, "bad signature") {
		t.Fatalf("refusal: %v", err)
	}
}
