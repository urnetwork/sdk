package sdk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/sdk/sn/evm"
	"github.com/urnetwork/sdk/sn/merkle"
	"github.com/urnetwork/sdk/sn/ss58"
)

// A payout artifact shaped like sn/payoutartifact's canonical json: the
// unsigned bytes are the same document with signer/content_hash/signature
// zeroed, and the content hash covers them.
type testArtifact struct {
	Schema          string             `json:"schema"`
	DeploymentID    string             `json:"deployment_id"`
	ChainID         uint64             `json:"chain_id"`
	Netuid          uint16             `json:"netuid"`
	Coordinator     string             `json:"coordinator"`
	SettlementVault string             `json:"settlement_vault"`
	Epoch           uint64             `json:"epoch"`
	NoID            uint64             `json:"no_id"`
	Start           map[string]any     `json:"start"`
	Providers       []map[string]any   `json:"providers"`
	Leaves          []testArtifactLeaf `json:"leaves"`
	PayoutRoot      [32]byte           `json:"payout_root"`
	SharesTotalBPS  uint64             `json:"shares_total_bps"`
	CreatedAt       string             `json:"created_at"`
	Signer          string             `json:"signer"`
	ContentHash     string             `json:"content_hash"`
	Signature       string             `json:"signature"`
}

type testArtifactLeaf struct {
	Index    uint64     `json:"index"`
	ClientID [16]byte   `json:"allocation_client_id"`
	Coldkey  [32]byte   `json:"coldkey"`
	ShareBPS uint64     `json:"share_bps"`
	Proof    [][32]byte `json:"proof"`
}

func buildTestArtifact(t *testing.T, epoch, noId uint64, coldkeys [][32]byte, shares []uint64) (raw []byte, contentHash [32]byte, root [32]byte) {
	t.Helper()
	leaves := make([]merkle.Leaf, len(coldkeys))
	for i := range coldkeys {
		leaves[i] = merkle.PayoutLeaf(coldkeys[i], new(big.Int).SetUint64(shares[i]))
	}
	tree, err := merkle.NewTree(leaves)
	if err != nil {
		t.Fatal(err)
	}
	root = tree.Root()
	artifact := testArtifact{
		Schema: "urnetwork-payout-artifact-v1", DeploymentID: "test", ChainID: 945, Netuid: 25,
		Coordinator: "0x1111111111111111111111111111111111111111", SettlementVault: "0x2222222222222222222222222222222222222222",
		Epoch: epoch, NoID: noId,
		Start:      map[string]any{"number": 1, "hash": "0xab"},
		Providers:  []map[string]any{{"client_id": []int{1, 2}, "signer": "nested-not-top-level"}},
		PayoutRoot: root, SharesTotalBPS: 10000, CreatedAt: "2026-09-02T00:00:00Z",
	}
	for i := range coldkeys {
		proof, err := tree.Proof(leaves[i])
		if err != nil {
			t.Fatal(err)
		}
		artifact.Leaves = append(artifact.Leaves, testArtifactLeaf{Index: uint64(i), Coldkey: coldkeys[i], ShareBPS: shares[i], Proof: proof})
	}
	unsigned := artifact
	unsigned.Signer = "0x0000000000000000000000000000000000000000"
	unsignedBytes, err := json.Marshal(unsigned)
	if err != nil {
		t.Fatal(err)
	}
	contentHash = sha256.Sum256(unsignedBytes)
	artifact.Signer = "0x9d8a62f656a8d1615c1294fd71e9cfb3e4855a4f"
	artifact.ContentHash = "sha256:" + hex.EncodeToString(contentHash[:])
	artifact.Signature = "0x" + strings.Repeat("ab", 65)
	raw, err = json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	return raw, contentHash, root
}

func testColdkey(seed byte) [32]byte {
	var k [32]byte
	for i := range k {
		k[i] = seed + byte(i)
	}
	return k
}

func TestArtifactUnsignedBytesAndParse(t *testing.T) {
	alice := testColdkey(1)
	bob := testColdkey(0x40)
	raw, contentHash, root := buildTestArtifact(t, 7, 3, [][32]byte{alice, bob}, []uint64{1234, 8766})

	unsigned, err := snArtifactUnsignedBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(unsigned); sum != contentHash {
		t.Fatalf("unsigned bytes hash mismatch\n%s", unsigned)
	}
	artifact, err := snParsePayoutArtifact(raw, contentHash, 7, big.NewInt(3), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Leaves) != 2 || artifact.Leaves[0].Coldkey != alice || artifact.Leaves[0].ShareBPS != 1234 {
		t.Fatalf("leaves %+v", artifact.Leaves)
	}
	if !merkle.Verify(root, merkle.PayoutLeaf(alice, big.NewInt(1234)), artifact.Leaves[0].Proof) {
		t.Fatal("proof from the parsed artifact does not verify")
	}
	// trailing newline tolerated
	if _, err := snParsePayoutArtifact(append(raw, '\n'), contentHash, 7, big.NewInt(3), root); err != nil {
		t.Fatal(err)
	}
	// tampering is caught by the chain hash
	tampered := bytes.Replace(raw, []byte(`"share_bps":1234`), []byte(`"share_bps":9999`), 1)
	if _, err := snParsePayoutArtifact(tampered, contentHash, 7, big.NewInt(3), root); err == nil {
		t.Fatal("tampered artifact accepted")
	}
	// wrong epoch / operator / root
	if _, err := snParsePayoutArtifact(raw, contentHash, 8, big.NewInt(3), root); err == nil {
		t.Fatal("wrong epoch accepted")
	}
	if _, err := snParsePayoutArtifact(raw, contentHash, 7, big.NewInt(4), root); err == nil {
		t.Fatal("wrong operator accepted")
	}
	var otherRoot [32]byte
	otherRoot[0] = 1
	if _, err := snParsePayoutArtifact(raw, contentHash, 7, big.NewInt(3), otherRoot); err == nil {
		t.Fatal("wrong root accepted")
	}
	// missing fields
	if _, err := snArtifactUnsignedBytes([]byte(`{"epoch":1}`)); err == nil {
		t.Fatal("artifact without signature fields accepted")
	}
	if _, err := snArtifactUnsignedBytes([]byte(`[1]`)); err == nil {
		t.Fatal("non-object accepted")
	}
}

// fakeChain answers the JSON-RPC calls the claims engine makes.
type fakeChain struct {
	t            *testing.T
	block        uint64
	currentEpoch int64
	vault        [20]byte
	coordinator  [20]byte
	entitlements map[int64]*evm.Entitlement
	claimed      map[string]bool
	sent         [][]byte
	receipts     map[string]bool
}

func (self *fakeChain) handle(method string, params []json.RawMessage) (any, *evm.RpcError) {
	switch method {
	case "eth_blockNumber":
		return evm.HexQuantity(new(big.Int).SetUint64(self.block)), nil
	case "eth_chainId":
		return "0x3b1", nil
	case "eth_gasPrice":
		return "0x3b9aca00", nil
	case "eth_getBalance":
		return "0xde0b6b3a7640000", nil // 1 TAO
	case "eth_getTransactionCount":
		return "0x5", nil
	case "eth_estimateGas":
		return "0x186a0", nil
	case "eth_sendRawTransaction":
		var raw string
		json.Unmarshal(params[0], &raw)
		b, _ := evm.ParseHexBytes(raw)
		self.sent = append(self.sent, b)
		h := evm.Keccak256(b)
		hash := "0x" + hex.EncodeToString(h[:])
		self.receipts[hash] = true
		return hash, nil
	case "eth_getTransactionReceipt":
		var hash string
		json.Unmarshal(params[0], &hash)
		if !self.receipts[hash] {
			return nil, nil
		}
		return map[string]any{"transactionHash": hash, "status": "0x1", "blockNumber": "0x10", "gasUsed": "0x5208"}, nil
	case "eth_call":
		var msg struct {
			To   string `json:"to"`
			Data string `json:"data"`
		}
		json.Unmarshal(params[0], &msg)
		data, _ := evm.ParseHexBytes(msg.Data)
		to, _ := evm.ParseAddress(msg.To)
		sel := hex.EncodeToString(data[:4])
		entitlementSel := evm.Selector("entitlement(uint256,uint256)")
		leafClaimedSel := evm.Selector("leafClaimed(uint256,bytes32)")
		switch {
		case to == self.coordinator && sel == hex.EncodeToString(evm.PackCurrentEpoch()):
			w, _ := evm.UintWord(big.NewInt(self.currentEpoch))
			return "0x" + hex.EncodeToString(w[:]), nil
		case to == self.vault && sel == hex.EncodeToString(entitlementSel[:]):
			epoch := new(big.Int).SetBytes(data[4:36]).Int64()
			ent := self.entitlements[epoch]
			if ent == nil {
				ent = &evm.Entitlement{Funded: big.NewInt(0), Total: big.NewInt(0), Claimed: big.NewInt(0)}
			}
			var out []byte
			out = append(out, ent.PayoutRoot[:]...)
			out = append(out, ent.ArtifactHash[:]...)
			for _, v := range []*big.Int{ent.Funded, ent.Total, ent.Claimed, new(big.Int).SetUint64(ent.ExpiryBlock), big.NewInt(int64(ent.Status))} {
				w, _ := evm.UintWord(v)
				out = append(out, w[:]...)
			}
			return "0x" + hex.EncodeToString(out), nil
		case to == self.vault && sel == hex.EncodeToString(leafClaimedSel[:]):
			key := hex.EncodeToString(data[4:68])
			w, _ := evm.UintWord(big.NewInt(0))
			if self.claimed[key] {
				w[31] = 1
			}
			return "0x" + hex.EncodeToString(w[:]), nil
		}
		return nil, &evm.RpcError{Code: 3, Message: "execution reverted"}
	}
	return nil, &evm.RpcError{Code: -32601, Message: "method not found: " + method}
}

func (self *fakeChain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := readAllBody(r)
	type req struct {
		Id     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	respond := func(rq req) map[string]any {
		result, rpcErr := self.handle(rq.Method, rq.Params)
		res := map[string]any{"jsonrpc": "2.0", "id": rq.Id}
		if rpcErr != nil {
			res["error"] = map[string]any{"code": rpcErr.Code, "message": rpcErr.Message}
		} else {
			res["result"] = result
		}
		return res
	}
	w.Header().Set("Content-Type", "application/json")
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("[")) {
		var reqs []req
		json.Unmarshal(body, &reqs)
		out := []map[string]any{}
		for _, rq := range reqs {
			out = append(out, respond(rq))
		}
		json.NewEncoder(w).Encode(out)
		return
	}
	var rq req
	json.Unmarshal(body, &rq)
	json.NewEncoder(w).Encode(respond(rq))
}

func readAllBody(r *http.Request) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}

func TestClaimsEngineEndToEnd(t *testing.T) {
	alice := testColdkey(1)
	bob := testColdkey(0x40)
	aliceSs58, err := ss58.Encode(alice, SnSs58Prefix)
	if err != nil {
		t.Fatal(err)
	}
	raw7, hash7, root7 := buildTestArtifact(t, 7, 3, [][32]byte{alice, bob}, []uint64{1234, 8766})
	raw6, hash6, root6 := buildTestArtifact(t, 6, 3, [][32]byte{bob}, []uint64{10000})
	raw5, hash5, root5 := buildTestArtifact(t, 5, 3, [][32]byte{alice}, []uint64{5000})

	artifacts := map[string][]byte{
		hex.EncodeToString(hash7[:]): raw7,
		hex.EncodeToString(hash6[:]): raw6,
		hex.EncodeToString(hash5[:]): raw5,
	}
	artifactServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sn/artifact" {
			http.NotFound(w, r)
			return
		}
		hash := strings.TrimPrefix(r.URL.Query().Get("hash"), "sha256:")
		raw, ok := artifacts[hash]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw)
	}))
	defer artifactServer.Close()

	vault, _ := evm.ParseAddress("0x2222222222222222222222222222222222222222")
	coordinator, _ := evm.ParseAddress("0x1111111111111111111111111111111111111111")
	claimKey5, _ := evm.ClaimKey(big.NewInt(3), alice)
	chain := &fakeChain{
		t: t, block: 1000, currentEpoch: 9, vault: vault, coordinator: coordinator,
		entitlements: map[int64]*evm.Entitlement{
			7: {PayoutRoot: root7, ArtifactHash: hash7, Funded: big.NewInt(1_000_000_000), Total: big.NewInt(1_000_000_000), Claimed: big.NewInt(0), ExpiryBlock: 5000, Status: evm.StatusFinalized},
			6: {PayoutRoot: root6, ArtifactHash: hash6, Funded: big.NewInt(1), Total: big.NewInt(1), Claimed: big.NewInt(0), ExpiryBlock: 5000, Status: evm.StatusFinalized},
			5: {PayoutRoot: root5, ArtifactHash: hash5, Funded: big.NewInt(2_000_000_000), Total: big.NewInt(2_000_000_000), Claimed: big.NewInt(1_000_000_000), ExpiryBlock: 5000, Status: evm.StatusFinalized},
			4: {Funded: big.NewInt(1), Total: big.NewInt(1), Claimed: big.NewInt(0), ExpiryBlock: 900, Status: evm.StatusCarried},
			8: {Funded: big.NewInt(1), Total: big.NewInt(1), Claimed: big.NewInt(0), Status: evm.StatusFunded},
		},
		claimed:  map[string]bool{fmt.Sprintf("%064x%s", 5, hex.EncodeToString(claimKey5[:])): true},
		receipts: map[string]bool{},
	}
	chainServer := httptest.NewServer(chain)
	defer chainServer.Close()

	settings := SnTestnetChainSettings()
	settings.SetRpcUrls(nil)
	settings.AddRpcUrl(chainServer.URL)
	settings.VaultAddress = "0x2222222222222222222222222222222222222222"
	settings.CoordinatorAddress = "0x1111111111111111111111111111111111111111"
	settings.NoId = "3"
	settings.ArtifactBaseUrl = artifactServer.URL
	if !settings.IsConfigured() {
		t.Fatal("settings not configured")
	}

	engine, snErr := newSnClaimEngine(settings, aliceSs58, "", nil, nil, nil)
	if snErr != nil {
		t.Fatal(snErr.Message)
	}
	result := engine.claims(context.Background(), 0)
	if result.Error != nil {
		t.Fatalf("claims error: %+v", result.Error)
	}
	if result.CurrentEpoch != 9 || result.BlockNumber != 1000 {
		t.Fatalf("head %+v", result)
	}
	byEpoch := map[int64]*SnEpochClaim{}
	for _, claim := range result.Claims.values {
		byEpoch[claim.Epoch] = claim
	}
	// newest first
	if result.Claims.Len() < 2 || result.Claims.Get(0).Epoch < result.Claims.Get(1).Epoch {
		t.Fatalf("claims not newest first: %+v", result.Claims.values)
	}
	if c := byEpoch[7]; c == nil || c.Status != SnClaimStatusClaimable || c.ShareBps != 1234 || c.AmountRao != 123_400_000 {
		t.Fatalf("epoch 7: %+v", c)
	}
	if c := byEpoch[5]; c == nil || c.Status != SnClaimStatusClaimed || c.AmountRao != 1_000_000_000 {
		t.Fatalf("epoch 5: %+v", c)
	}
	if c := byEpoch[6]; c != nil {
		t.Fatalf("epoch 6 has no share for alice but was listed: %+v", c)
	}
	if c := byEpoch[8]; c == nil || c.Status != SnClaimStatusOpen {
		t.Fatalf("epoch 8: %+v", c)
	}
	if c := byEpoch[9]; c == nil || c.Status != SnClaimStatusOpen {
		t.Fatalf("epoch 9 (current, unset): %+v", c)
	}
	if c := byEpoch[4]; c != nil {
		t.Fatalf("old carried epoch listed: %+v", c)
	}
	if result.TotalClaimableRao != 123_400_000 {
		t.Fatalf("total claimable %d", result.TotalClaimableRao)
	}

	// unsigned transactions for an external signer
	epochs := NewInt64List()
	epochs.Add(7)
	epochs.Add(5)
	txs, err := engine.unsignedTxs(context.Background(), epochs.values)
	if err != nil {
		t.Fatal(err)
	}
	if txs.Len() != 1 || txs.Get(0).Epoch != 7 || txs.Get(0).ChainId != 945 || !strings.EqualFold(txs.Get(0).To, settings.VaultAddress) {
		t.Fatalf("txs %+v", txs.values)
	}
	wantData, _ := evm.PackClaim(big.NewInt(7), big.NewInt(3), alice, big.NewInt(1234), byEpoch7Proof(t, raw7))
	if txs.Get(0).Data != "0x"+hex.EncodeToString(wantData) {
		t.Fatal("calldata differs from PackClaim")
	}
	if _, err := engine.unsignedTxs(context.Background(), []int64{5}); err == nil || !strings.Contains(err.Error(), SnErrorCodeAlreadyClaimed) {
		t.Fatalf("claimed epoch built a tx: %v", err)
	}

	// send with a gas key against the fake chain
	key, err := evm.PrivateKeyFromHex("0x4646464646464646464646464646464646464646464646464646464646464646")
	if err != nil {
		t.Fatal(err)
	}
	cb := &recordingClaimCallback{}
	engine.send(context.Background(), key, 7, cb)
	if len(cb.failed) != 0 || len(cb.sent) != 1 || len(cb.confirmed) != 1 || cb.confirmed[0].amount != 123_400_000 {
		t.Fatalf("send: %+v", cb)
	}
	if len(chain.sent) != 1 {
		t.Fatal("no raw tx reached the chain")
	}
	// the raw tx is a legacy EIP-155 tx from the gas key carrying the claim calldata
	if !bytes.Contains(chain.sent[0], wantData) {
		t.Fatal("raw tx does not carry the claim calldata")
	}
	if engine.txHashes[7] != cb.sent[0].txHash {
		t.Fatal("tx hash not remembered")
	}
	engine.send(context.Background(), key, 5, cb)
	if len(cb.failed) != 1 || !strings.HasPrefix(cb.failed[0].message, SnErrorCodeAlreadyClaimed) {
		t.Fatalf("claimed epoch: %+v", cb.failed)
	}
}

func byEpoch7Proof(t *testing.T, raw []byte) [][32]byte {
	t.Helper()
	var artifact snPayoutArtifact
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatal(err)
	}
	return artifact.Leaves[0].Proof
}

type recordingClaimCallback struct {
	sent []struct {
		epoch  int64
		txHash string
	}
	confirmed []struct {
		epoch  int64
		txHash string
		amount int64
	}
	failed []struct {
		epoch   int64
		message string
	}
	done int
}

func (self *recordingClaimCallback) Sent(epoch int64, txHash string) {
	self.sent = append(self.sent, struct {
		epoch  int64
		txHash string
	}{epoch, txHash})
}
func (self *recordingClaimCallback) Confirmed(epoch int64, txHash string, amountRao int64) {
	self.confirmed = append(self.confirmed, struct {
		epoch  int64
		txHash string
		amount int64
	}{epoch, txHash, amountRao})
}
func (self *recordingClaimCallback) Failed(epoch int64, message string) {
	self.failed = append(self.failed, struct {
		epoch   int64
		message string
	}{epoch, message})
}
func (self *recordingClaimCallback) Done() { self.done++ }

func TestSnUtilities(t *testing.T) {
	alice := testColdkey(1)
	address, err := ss58.Encode(alice, SnSs58Prefix)
	if err != nil {
		t.Fatal(err)
	}
	if !ValidateSs58(address) || !ValidateSs58("  "+address+" ") {
		t.Fatal("valid address rejected")
	}
	for _, bad := range []string{"", "5F", address[:len(address)-1] + "x", "0x9d8A62f656a8d1615C1294fd71e9CFb3E4855A4F"} {
		if ValidateSs58(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
	// a polkadot-prefix (0) address of the same key is not a bittensor address
	polkadot, _ := ss58.Encode(alice, 0)
	if ValidateSs58(polkadot) {
		t.Fatal("prefix 0 accepted")
	}
	if short := ShortSs58(address); len([]rune(short)) != 9 || short[:4] != address[:4] || short[len(short)-4:] != address[len(address)-4:] {
		t.Fatalf("ShortSs58 = %q", short)
	}
	mirror := EvmMirrorSs58("0x9d8A62f656a8d1615C1294fd71e9CFb3E4855A4F")
	if mirror == "" || !ValidateSs58(mirror) {
		t.Fatalf("mirror %q", mirror)
	}
	if EvmMirrorSs58("nope") != "" {
		t.Fatal("bad address mirrored")
	}
	for rao, want := range map[int64]string{
		0: "0.0000", 3_241_000_000: "3.2410", 3_241_049_999: "3.2410", 3_241_050_000: "3.2411",
		999_999_950_000: "1000.0000", 1: "0.0000", -1_500_000_000: "-1.5000",
	} {
		if got := FormatAlphaAmount(rao); got != want {
			t.Fatalf("FormatAlphaAmount(%d) = %q want %q", rao, got, want)
		}
	}
	if got := FormatAlpha(3_241_000_000); got != "3.2410 SN25α" {
		t.Fatalf("FormatAlpha = %q", got)
	}
	if got := FormatShareBps(71); got != "0.71%" {
		t.Fatalf("FormatShareBps = %q", got)
	}
	if got := FormatShareBps(10000); got != "100.00%" {
		t.Fatalf("FormatShareBps = %q", got)
	}
	// proof helpers agree with the merkle package
	bob := testColdkey(0x40)
	leaves := []merkle.Leaf{merkle.PayoutLeaf(alice, big.NewInt(1234)), merkle.PayoutLeaf(bob, big.NewInt(8766))}
	tree, _ := merkle.NewTree(leaves)
	proof, _ := tree.Proof(leaves[0])
	root := tree.Root()
	proofBytes := make([][]byte, len(proof))
	proofHex := NewStringList()
	for i := range proof {
		proofBytes[i] = proof[i][:]
		proofHex.Add("0x" + hex.EncodeToString(proof[i][:]))
	}
	if !verifyPayoutProof(root[:], leaves[0][:], proofBytes) {
		t.Fatal("verifyPayoutProof rejected a valid proof")
	}
	if !VerifyPayoutProofHex("0x"+hex.EncodeToString(root[:]), SnPayoutLeafHex(address, 1234), proofHex) {
		t.Fatal("VerifyPayoutProofHex rejected a valid proof")
	}
	if VerifyPayoutProofHex("0x"+hex.EncodeToString(root[:]), SnPayoutLeafHex(address, 1235), proofHex) {
		t.Fatal("wrong share verified")
	}
}

func TestSnChainSettingsJsonAndMerge(t *testing.T) {
	base := DefaultSnChainSettings()
	if base.ChainId != SnChainIdMainnet || base.RpcUrls().Len() != 1 || base.IsConfigured() {
		t.Fatalf("defaults %+v", base)
	}
	overrides := NewSnChainSettings()
	overrides.VaultAddress = "0x2222222222222222222222222222222222222222"
	overrides.CoordinatorAddress = "0x1111111111111111111111111111111111111111"
	overrides.NoId = "0x3"
	overrides.AddRpcUrl("https://rpc.example")
	merged := base.Merge(overrides)
	if !merged.IsConfigured() || merged.RpcUrls().Get(0) != "https://rpc.example" || merged.ChainId != SnChainIdMainnet {
		t.Fatalf("merged %+v", merged)
	}
	if base.IsConfigured() {
		t.Fatal("merge mutated the base")
	}
	b, err := json.Marshal(merged)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"rpc_urls":["https://rpc.example"]`) {
		t.Fatalf("json %s", b)
	}
	var back SnChainSettings
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.IsConfigured() || back.RpcUrls().Len() != 1 {
		t.Fatalf("round trip %+v", back)
	}
	noId, _ := back.noId()
	if noId.Int64() != 3 {
		t.Fatalf("noId %s", noId)
	}
	if got := back.ExplorerUrlForTx("0xabc"); got != "https://evm.taostats.io/tx/0xabc" {
		t.Fatalf("explorer %q", got)
	}
}

func TestSnHeadResultBoundShapes(t *testing.T) {
	var flag SnHeadResult
	if err := json.Unmarshal([]byte(`{"eligible":true,"score":1.5,"rank_estimate":12,"cutoff":200,"bound":true,"hotkey":"5F","uid":7,"epoch":9,"source":"server"}`), &flag); err != nil {
		t.Fatal(err)
	}
	if !flag.Eligible || !flag.Bound || flag.Hotkey != "5F" || flag.Uid != 7 || flag.RankEstimate != 12 {
		t.Fatalf("%+v", flag)
	}
	var obj SnHeadResult
	if err := json.Unmarshal([]byte(`{"eligible":false,"bound":{"hotkey":"5G","uid":3},"epoch":9,"source":"chain"}`), &obj); err != nil {
		t.Fatal(err)
	}
	if !obj.Bound || obj.Hotkey != "5G" || obj.Uid != 3 || obj.Source != "chain" {
		t.Fatalf("%+v", obj)
	}
	var none SnHeadResult
	if err := json.Unmarshal([]byte(`{"eligible":false,"bound":null,"error":{"message":"nope"}}`), &none); err != nil {
		t.Fatal(err)
	}
	if none.Bound || none.Error == nil || none.Error.Message != "nope" {
		t.Fatalf("%+v", none)
	}
}

func TestSnFleetBindingDigestFromJson(t *testing.T) {
	// the sn/protocol golden, fields as hex strings
	bindingJson := `{"chain_id":945,"netuid":17,"coordinator":"0x1111111111111111111111111111111111111111","fleet_id":"0x2222222222222222222222222222222222222222222222222222222222222222","hotkey":"0x94ad8d1ead1a2bff9bbbac89aa89b13df2fe9ec929a09c90bc5ddb1dff723b47","client_id":"0x33333333333333333333333333333333","client_key":"0x03a107bff3ce10be1d70dd18e74bc09967e4d6309ba50d5f1ddc8664125531b8","generation":3,"valid_from_epoch":11,"valid_to_epoch":42,"commitment_hash":"0x4444444444444444444444444444444444444444444444444444444444444444"}`
	digest, err := SnFleetBindingDigest(bindingJson)
	if err != nil {
		t.Fatal(err)
	}
	if digest != "0x0de356fd56fc28d72efe5724a81b2462a7f2bb3f041f48128e2d511b0ae05ba7" {
		t.Fatalf("digest %s", digest)
	}
	// the same binding with byte arrays, string numbers and a uuid client id
	arrayJson := `{"chain_id":"945","netuid":"17","coordinator":[17,17,17,17,17,17,17,17,17,17,17,17,17,17,17,17,17,17,17,17],"fleet_id":"2222222222222222222222222222222222222222222222222222222222222222","hotkey":"0x94ad8d1ead1a2bff9bbbac89aa89b13df2fe9ec929a09c90bc5ddb1dff723b47","client_id":"33333333-3333-3333-3333-333333333333","client_key":"0x03a107bff3ce10be1d70dd18e74bc09967e4d6309ba50d5f1ddc8664125531b8","generation":"3","valid_from_epoch":11,"valid_to_epoch":42,"commitment_hash":"0x4444444444444444444444444444444444444444444444444444444444444444"}`
	digest2, err := SnFleetBindingDigest(arrayJson)
	if err != nil {
		t.Fatal(err)
	}
	if digest2 != digest {
		t.Fatalf("digest from array form %s != %s", digest2, digest)
	}
	if _, err := SnFleetBindingDigest(`{"chain_id":0}`); err == nil {
		t.Fatal("invalid binding digested")
	}
}
