package sdk

// Settlement v2 claims (APPS-SPEC.md, the pinned SDK v2 claim API and its
// claim rules 1 to 5). The ledger endpoint is an httptest server, and vault2
// a JSON-RPC fake that runs claim() on the roots and watermarks it holds,
// answers reverts as the Subtensor node does, and recovers each sent
// transaction's sender from its signature.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026/sn/evm"
	"github.com/urnetwork/sdk/v2026/sn/merkle"
	"github.com/urnetwork/sdk/v2026/sn/ss58"
)

const testVault2 = "0x2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b"

// a sum tree leaf of the test ledgers
type testLedgerLeaf struct {
	coldkey    [32]byte
	cumulative int64
}

// buildTestLedgerTree builds the urnetwork-ledger-sum-tree-v1 of leaves, with
// padding positions (0x00…00, 0), and every account's bottom-up proof.
func buildTestLedgerTree(leaves []testLedgerLeaf) ([32]byte, *big.Int, [][]merkle.LedgerNode) {
	coldkeys := make([][32]byte, len(leaves))
	values := make([]*big.Int, len(leaves))
	for i, leaf := range leaves {
		coldkeys[i], values[i] = leaf.coldkey, big.NewInt(leaf.cumulative)
	}
	return buildTestLedgerSumTree(coldkeys, values)
}

func buildTestLedgerSumTree(coldkeys [][32]byte, values []*big.Int) ([32]byte, *big.Int, [][]merkle.LedgerNode) {
	d, err := merkle.LedgerTreeDepth(uint64(len(coldkeys)))
	if err != nil {
		panic(err)
	}
	level := make([]merkle.LedgerNode, 1<<d)
	for i := range level {
		level[i] = merkle.LedgerNode{Sum: big.NewInt(0)}
	}
	for i := range coldkeys {
		level[i] = merkle.LedgerNode{Hash: merkle.LedgerLeafHash(coldkeys[i], values[i]), Sum: values[i]}
	}
	levels := [][]merkle.LedgerNode{level}
	for k := 0; k < d; k++ {
		below := levels[k]
		above := make([]merkle.LedgerNode, len(below)/2)
		for j := range above {
			left, right := below[2*j], below[2*j+1]
			above[j] = merkle.LedgerNode{Hash: merkle.LedgerNodeHash(left.Hash, left.Sum, right.Hash, right.Sum), Sum: new(big.Int).Add(left.Sum, right.Sum)}
		}
		levels = append(levels, above)
	}
	proofs := make([][]merkle.LedgerNode, len(coldkeys))
	for i := range coldkeys {
		proofs[i] = []merkle.LedgerNode{}
		for k := 0; k < d; k++ {
			proofs[i] = append(proofs[i], levels[k][(i>>k)^1])
		}
	}
	return levels[d][0].Hash, levels[d][0].Sum, proofs
}

// The tree builder the claim tests use reproduces the sn vector trees.
func TestBuildTestLedgerTreeMatchesTheVectors(t *testing.T) {
	raw, err := os.ReadFile("sn/merkle/testdata/ledger_v2_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Trees []struct {
			Name     string `json:"name"`
			Accounts []struct {
				Coldkey    string `json:"coldkey"`
				Cumulative string `json:"cumulative"`
			} `json:"accounts"`
			Root  string `json:"root"`
			Total string `json:"total"`
		} `json:"trees"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Trees) == 0 {
		t.Fatal("no vector trees")
	}
	for _, tree := range file.Trees {
		var coldkeys [][32]byte
		var values []*big.Int
		for _, account := range tree.Accounts {
			coldkey, err := evm.ParseHash32(account.Coldkey)
			value, ok := new(big.Int).SetString(account.Cumulative, 10)
			if err != nil || !ok {
				t.Fatalf("%s: account %+v", tree.Name, account)
			}
			coldkeys, values = append(coldkeys, coldkey), append(values, value)
		}
		root, total, _ := buildTestLedgerSumTree(coldkeys, values)
		if "0x"+hex.EncodeToString(root[:]) != tree.Root || total.String() != tree.Total {
			t.Fatalf("%s: root %x total %s", tree.Name, root, total)
		}
	}
}

type fakeVault2Root struct {
	root      [32]byte
	total     *big.Int
	leafCount uint64
}

type fakeVault2Answer struct {
	result any
	err    *evm.RpcError
}

type fakeVault2Call struct {
	method   string
	from     string
	to       string
	selector string
}

// a transaction the fake received, decoded, with its recovered sender
type fakeVault2Tx struct {
	dynamic  bool
	nonce    uint64
	gasPrice *big.Int
	tip      *big.Int
	feeCap   *big.Int
	gas      uint64
	to       [20]byte
	value    *big.Int
	data     []byte
	from     [20]byte
}

// fakeVault2 answers the JSON-RPC calls of a v2 claim as STSettlementVault2
// on a Subtensor node.
type fakeVault2 struct {
	t      *testing.T
	mutex  sync.Mutex
	vault2 [20]byte
	escrow [32]byte
	// vault2's own coldkey, the ss58 mirror of its address, which a claim
	// may not pay
	selfColdkey [32]byte
	chainId     *big.Int
	roots       map[int64][]fakeVault2Root
	claimed     map[[2]int64]*big.Int
	// a custom error vault2 refuses every claim with, or the runtime error the
	// staking precompile refuses the transfer with, which reverts without data
	vaultRefusal   string
	runtimeRefusal string
	estimate       uint64
	gasPrice       *big.Int
	baseFee        *big.Int
	tip            *big.Int
	balance        *big.Int
	nonce          uint64
	// run once, before the next claim eth_call or the next sent transaction
	beforeClaimCall func()
	beforeMine      func()
	// a mined claim's receipt has no Claimed log
	dropClaimedLog bool
	// methods answered with an error
	failing map[string]*evm.RpcError
	// eth_call and eth_estimateGas answers by selector, in place of vault2's
	bySelector map[string]fakeVault2Answer
	// a batch's answer leaves out the calls of this selector
	omitFromBatch string
	calls         []fakeVault2Call
	sent          []*fakeVault2Tx
	receipts      map[string]any
}

func newFakeVault2(t *testing.T) *fakeVault2 {
	vault2, _ := evm.ParseAddress(testVault2)
	selfColdkey, err := snColdkeyBytes(EvmMirrorSs58(testVault2))
	if err != nil {
		t.Fatal(err)
	}
	return &fakeVault2{
		t:           t,
		vault2:      vault2,
		escrow:      testColdkey(0x33),
		selfColdkey: selfColdkey,
		chainId:     big.NewInt(SnChainIdTestnet),
		roots:       map[int64][]fakeVault2Root{},
		claimed:     map[[2]int64]*big.Int{},
		estimate:    2_514_273,
		gasPrice:    big.NewInt(10_000_000_000),
		tip:         big.NewInt(2_000_000_000),
		balance:     new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),
		nonce:       5,
		failing:     map[string]*evm.RpcError{},
		bySelector:  map[string]fakeVault2Answer{},
		receipts:    map[string]any{},
	}
}

func (self *fakeVault2) claimedOf(noId int64, index int64) *big.Int {
	if paid := self.claimed[[2]int64{noId, index}]; paid != nil {
		return paid
	}
	return big.NewInt(0)
}

// paid reads vault2's claimed(noId, index).
func (self *fakeVault2) paid(noId int64, index int64) int64 {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.claimedOf(noId, index).Int64()
}

func (self *fakeVault2) setClaimed(noId int64, index int64, claimed int64) {
	self.claimed[[2]int64{noId, index}] = big.NewInt(claimed)
}

func (self *fakeVault2) methods() []string {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	var methods []string
	for _, call := range self.calls {
		methods = append(methods, call.method)
	}
	return methods
}

func (self *fakeVault2) sentTxs() []*fakeVault2Tx {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return append([]*fakeVault2Tx(nil), self.sent...)
}

// the calls of claim() and where they came from
func (self *fakeVault2) claimCalls() []fakeVault2Call {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	var calls []fakeVault2Call
	for _, call := range self.calls {
		if call.selector == "b7a6b907" {
			calls = append(calls, call)
		}
	}
	return calls
}

func testSelectorData(signature string) []byte {
	sel := evm.Selector(signature)
	return sel[:]
}

type testLedgerClaimArgs struct {
	noId       int64
	rootIndex  int64
	index      int64
	coldkey    [32]byte
	cumulative *big.Int
	proof      []merkle.LedgerNode
}

// decodeTestLedgerClaim decodes claim(noId, rootIndex, account, proof)
// calldata, requiring the canonical encoding.
func decodeTestLedgerClaim(data []byte) (*testLedgerClaimArgs, error) {
	if len(data) < 4+7*32 || hex.EncodeToString(data[:4]) != "b7a6b907" {
		return nil, errors.New("not claim calldata")
	}
	word := func(i int) []byte { return data[4+32*i : 4+32*(i+1)] }
	number := func(i int) *big.Int { return new(big.Int).SetBytes(word(i)) }
	n := int(number(6).Int64())
	if number(5).Int64() != 6*32 || len(data) != 4+32*(7+2*n) {
		return nil, errors.New("claim calldata is not canonical")
	}
	c := &testLedgerClaimArgs{
		noId:       number(0).Int64(),
		rootIndex:  number(1).Int64(),
		index:      number(2).Int64(),
		coldkey:    [32]byte(word(3)),
		cumulative: number(4),
		proof:      []merkle.LedgerNode{},
	}
	for k := 0; k < n; k++ {
		c.proof = append(c.proof, merkle.LedgerNode{Hash: [32]byte(word(7 + 2*k)), Sum: number(8 + 2*k)})
	}
	return c, nil
}

// runClaim runs STSettlementVault2.claim on the fake's state, without its
// effects: the claim and the amount it pays, or its revert data, which is
// empty when the staking precompile refuses the transfer.
func (self *fakeVault2) runClaim(data []byte) (*testLedgerClaimArgs, *big.Int, []byte) {
	c, err := decodeTestLedgerClaim(data)
	if err != nil {
		self.t.Errorf("vault2 claim: %v", err)
		return nil, nil, testSelectorData("Malformed()")
	}
	roots := self.roots[c.noId]
	if c.rootIndex >= int64(len(roots)) {
		return nil, nil, testSelectorData("UnknownRoot()")
	}
	if c.coldkey == ([32]byte{}) || c.coldkey == self.selfColdkey {
		return nil, nil, testSelectorData("InvalidColdkey()")
	}
	root := roots[c.rootIndex]
	if !merkle.VerifyLedgerProof(root.root, root.total, root.leafCount, uint64(c.index), c.coldkey, c.cumulative, c.proof) {
		return nil, nil, testSelectorData("InvalidProof()")
	}
	paid := self.claimedOf(c.noId, c.index)
	if c.cumulative.Cmp(paid) <= 0 {
		return nil, nil, testSelectorData("NothingToClaim()")
	}
	if self.vaultRefusal != "" {
		return nil, nil, testSelectorData(self.vaultRefusal + "()")
	}
	if self.runtimeRefusal != "" {
		return nil, nil, []byte{}
	}
	return c, new(big.Int).Sub(c.cumulative, paid), nil
}

// a revert as the node answers it: code 3 and the revert data
func testRevert(data []byte) *evm.RpcError {
	return &evm.RpcError{Code: 3, Message: "VM Exception while processing transaction: revert", Data: json.RawMessage(`"0x` + hex.EncodeToString(data) + `"`)}
}

func testWordHex(v *big.Int) string {
	w, _ := evm.UintWord(v)
	return "0x" + hex.EncodeToString(w[:])
}

// testRootsHex is what roots() returns: the static Root tuple (root, total,
// leafCount, epoch, ledgerHash).
func testRootsHex(root [32]byte, total *big.Int, leafCount *big.Int, epoch *big.Int) string {
	ret := append([]byte(nil), root[:]...)
	for _, v := range []*big.Int{total, leafCount, epoch} {
		w, _ := evm.UintWord(v)
		ret = append(ret, w[:]...)
	}
	ret = append(ret, bytes.Repeat([]byte{0xee}, 32)...)
	return "0x" + hex.EncodeToString(ret)
}

func (self *fakeVault2) handle(method string, params []json.RawMessage) (any, *evm.RpcError) {
	call := fakeVault2Call{method: method}
	var data []byte
	if method == "eth_call" || method == "eth_estimateGas" {
		var msg struct {
			From string `json:"from"`
			To   string `json:"to"`
			Data string `json:"data"`
		}
		json.Unmarshal(params[0], &msg)
		data, _ = evm.ParseHexBytes(msg.Data)
		call.from, call.to = strings.ToLower(msg.From), strings.ToLower(msg.To)
		if len(data) >= 4 {
			call.selector = hex.EncodeToString(data[:4])
		}
	}
	self.calls = append(self.calls, call)
	if rpcErr := self.failing[method]; rpcErr != nil {
		return nil, rpcErr
	}
	switch method {
	case "eth_gasPrice":
		return evm.HexQuantity(self.gasPrice), nil
	case "eth_maxPriorityFeePerGas":
		return evm.HexQuantity(self.tip), nil
	case "eth_getBalance":
		return evm.HexQuantity(self.balance), nil
	case "eth_getTransactionCount":
		return evm.HexQuantity(new(big.Int).SetUint64(self.nonce)), nil
	case "eth_getBlockByNumber":
		block := map[string]any{"number": "0x10", "timestamp": "0x1"}
		if self.baseFee != nil {
			block["baseFeePerGas"] = evm.HexQuantity(self.baseFee)
		}
		return block, nil
	case "eth_call", "eth_estimateGas":
		return self.call(method, call, data)
	case "eth_sendRawTransaction":
		var rawHex string
		json.Unmarshal(params[0], &rawHex)
		raw, _ := evm.ParseHexBytes(rawHex)
		tx, err := decodeTestTx(raw, self.chainId)
		if err != nil {
			self.t.Errorf("sent transaction: %v", err)
			return nil, &evm.RpcError{Code: -32000, Message: err.Error()}
		}
		self.sent = append(self.sent, tx)
		if self.beforeMine != nil {
			hook := self.beforeMine
			self.beforeMine = nil
			hook()
		}
		hash := evm.Keccak256(raw)
		txHash := "0x" + hex.EncodeToString(hash[:])
		self.receipts[txHash] = self.mine(tx, txHash)
		return txHash, nil
	case "eth_getTransactionReceipt":
		var txHash string
		json.Unmarshal(params[0], &txHash)
		return self.receipts[txHash], nil
	}
	return nil, &evm.RpcError{Code: -32601, Message: "method not found: " + method}
}

func (self *fakeVault2) call(method string, call fakeVault2Call, data []byte) (any, *evm.RpcError) {
	if answer, ok := self.bySelector[call.selector]; ok {
		return answer.result, answer.err
	}
	vault2 := "0x" + hex.EncodeToString(self.vault2[:])
	switch {
	case call.to == vault2 && call.selector == "0fc3cec0":
		// roots(noId, rootIndex) panics past the pool's last root
		noId, rootIndex := new(big.Int).SetBytes(data[4:36]).Int64(), new(big.Int).SetBytes(data[36:68]).Int64()
		roots := self.roots[noId]
		if rootIndex >= int64(len(roots)) {
			return nil, testRevert(append(testSelectorData("Panic(uint256)"), make([]byte, 31)...))
		}
		root := roots[rootIndex]
		return testRootsHex(root.root, root.total, new(big.Int).SetUint64(root.leafCount), big.NewInt(9)), nil
	case call.to == vault2 && call.selector == "a54ab457":
		noId, index := new(big.Int).SetBytes(data[4:36]).Int64(), new(big.Int).SetBytes(data[36:68]).Int64()
		return testWordHex(self.claimedOf(noId, index)), nil
	case call.to == vault2 && call.selector == "164bf360":
		return "0x" + hex.EncodeToString(self.escrow[:]), nil
	case call.to == "0x0000000000000000000000000000000000000805" && call.from == vault2 && call.selector == "17ce5f62":
		// eth_call from vault2 runs the precompile as vault2's coldkey
		if self.runtimeRefusal != "" {
			return nil, &evm.RpcError{Code: -32603, Message: `evm error: Other("dispatch execution failed: ` + self.runtimeRefusal + `")`}
		}
		return "0x", nil
	case call.to == vault2 && call.selector == "b7a6b907":
		if self.beforeClaimCall != nil {
			hook := self.beforeClaimCall
			self.beforeClaimCall = nil
			hook()
		}
		_, amount, revert := self.runClaim(data)
		switch {
		case revert != nil:
			return nil, testRevert(revert)
		case method == "eth_estimateGas":
			return evm.HexQuantity(new(big.Int).SetUint64(self.estimate)), nil
		default:
			return testWordHex(amount), nil
		}
	}
	return nil, testRevert(nil)
}

// mine runs a sent claim: a refused one reverts with no effect and no log.
func (self *fakeVault2) mine(tx *fakeVault2Tx, txHash string) map[string]any {
	receipt := map[string]any{"transactionHash": txHash, "status": "0x0", "blockNumber": "0x11", "gasUsed": "0x39992", "logs": []any{}}
	if tx.to != self.vault2 || tx.value.Sign() != 0 {
		self.t.Errorf("a transaction to %x with value %s", tx.to, tx.value)
		return receipt
	}
	// the transaction consumes the nonce whatever it does
	self.nonce++
	c, amount, revert := self.runClaim(tx.data)
	if revert != nil {
		return receipt
	}
	self.claimed[[2]int64{c.noId, c.index}] = c.cumulative
	receipt["status"] = "0x1"
	if !self.dropClaimedLog {
		data := ""
		for _, v := range []*big.Int{big.NewInt(c.rootIndex), c.cumulative, amount, new(big.Int).SetBytes(tx.from[:])} {
			data += strings.TrimPrefix(testWordHex(v), "0x")
		}
		receipt["logs"] = []any{
			// another contract's log with the same shape is not vault2's
			map[string]any{"address": "0x" + strings.Repeat("99", 20), "data": "0x" + data, "topics": []string{
				"0x" + hex.EncodeToString(evm.LedgerClaimedTopic[:]), testWordHex(big.NewInt(c.noId)), testWordHex(big.NewInt(c.index)), "0x" + hex.EncodeToString(c.coldkey[:]),
			}},
			map[string]any{"address": "0x" + hex.EncodeToString(self.vault2[:]), "data": "0x" + data, "topics": []string{
				"0x" + hex.EncodeToString(evm.LedgerClaimedTopic[:]), testWordHex(big.NewInt(c.noId)), testWordHex(big.NewInt(c.index)), "0x" + hex.EncodeToString(c.coldkey[:]),
			}},
		}
	}
	return receipt
}

func (self *fakeVault2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	body, _ := readAllBody(r)
	type request struct {
		Id     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	respond := func(req request) map[string]any {
		result, rpcErr := self.handle(req.Method, req.Params)
		res := map[string]any{"jsonrpc": "2.0", "id": req.Id}
		if rpcErr != nil {
			rpcError := map[string]any{"code": rpcErr.Code, "message": rpcErr.Message}
			if rpcErr.Data != nil {
				rpcError["data"] = rpcErr.Data
			}
			res["error"] = rpcError
		} else {
			res["result"] = result
		}
		return res
	}
	w.Header().Set("Content-Type", "application/json")
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("[")) {
		var reqs []request
		json.Unmarshal(body, &reqs)
		out := []map[string]any{}
		for _, req := range reqs {
			res := respond(req)
			if self.omitFromBatch != "" && self.calls[len(self.calls)-1].selector == self.omitFromBatch {
				continue
			}
			out = append(out, res)
		}
		json.NewEncoder(w).Encode(out)
		return
	}
	var req request
	json.Unmarshal(body, &req)
	json.NewEncoder(w).Encode(respond(req))
}

// rlp, enough to decode a signed transaction

func testRlpSplit(b []byte) (payload []byte, isList bool, rest []byte, err error) {
	if len(b) == 0 {
		return nil, false, nil, errors.New("rlp: empty")
	}
	prefix := b[0]
	start, size := 0, 0
	switch {
	case prefix < 0x80:
		return b[:1], false, b[1:], nil
	case prefix < 0xb8:
		start, size = 1, int(prefix-0x80)
	case prefix < 0xc0:
		n := int(prefix - 0xb7)
		start, size = 1+n, int(new(big.Int).SetBytes(b[1:1+n]).Int64())
	case prefix < 0xf8:
		start, size, isList = 1, int(prefix-0xc0), true
	default:
		n := int(prefix - 0xf7)
		start, size, isList = 1+n, int(new(big.Int).SetBytes(b[1:1+n]).Int64()), true
	}
	if len(b) < start+size {
		return nil, false, nil, errors.New("rlp: truncated")
	}
	return b[start : start+size], isList, b[start+size:], nil
}

func testRlpList(b []byte) ([][]byte, error) {
	payload, isList, rest, err := testRlpSplit(b)
	if err != nil || !isList || len(rest) != 0 {
		return nil, fmt.Errorf("rlp: not one list: %v", err)
	}
	var items [][]byte
	for len(payload) > 0 {
		item, _, next, err := testRlpSplit(payload)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		payload = next
	}
	return items, nil
}

// decodeTestTx decodes a signed legacy (EIP-155) or EIP-1559 transaction and
// recovers its sender.
func decodeTestTx(raw []byte, chainId *big.Int) (*fakeVault2Tx, error) {
	number := func(b []byte) *big.Int { return new(big.Int).SetBytes(b) }
	tx := &fakeVault2Tx{}
	var hash [32]byte
	var sig *evm.Signature
	if len(raw) > 0 && raw[0] == 0x02 {
		items, err := testRlpList(raw[1:])
		if err != nil || len(items) != 12 || number(items[0]).Cmp(chainId) != 0 || len(items[8]) != 0 {
			return nil, fmt.Errorf("not an EIP-1559 transaction on chain %s: %v", chainId, err)
		}
		tx.dynamic = true
		tx.nonce, tx.tip, tx.feeCap, tx.gas = number(items[1]).Uint64(), number(items[2]), number(items[3]), number(items[4]).Uint64()
		tx.to, tx.value, tx.data = [20]byte(items[5]), number(items[6]), items[7]
		hash = (&evm.DynamicFeeTx{Nonce: tx.nonce, MaxPriorityFeePerGas: tx.tip, MaxFeePerGas: tx.feeCap, Gas: tx.gas, To: tx.to, Value: tx.value, Data: tx.data}).SigningHash(chainId)
		sig = &evm.Signature{R: number(items[10]), S: number(items[11]), RecId: byte(number(items[9]).Uint64())}
	} else {
		items, err := testRlpList(raw)
		if err != nil || len(items) != 9 {
			return nil, fmt.Errorf("not a legacy transaction: %v", err)
		}
		tx.nonce, tx.gasPrice, tx.gas = number(items[0]).Uint64(), number(items[1]), number(items[2]).Uint64()
		tx.to, tx.value, tx.data = [20]byte(items[3]), number(items[4]), items[5]
		// EIP-155: v = chainId × 2 + 35 + recId
		recId := new(big.Int).Sub(number(items[6]), new(big.Int).Add(new(big.Int).Mul(chainId, big.NewInt(2)), big.NewInt(35)))
		if !recId.IsUint64() || recId.Uint64() > 1 {
			return nil, fmt.Errorf("v %s is not EIP-155 on chain %s", number(items[6]), chainId)
		}
		hash = (&evm.LegacyTx{Nonce: tx.nonce, GasPrice: tx.gasPrice, Gas: tx.gas, To: tx.to, Value: tx.value, Data: tx.data}).SigningHash(chainId)
		sig = &evm.Signature{R: number(items[7]), S: number(items[8]), RecId: byte(recId.Uint64())}
	}
	from, err := evm.Recover(hash, sig)
	if err != nil {
		return nil, err
	}
	tx.from = from
	return tx, nil
}

// the ledger answer

func testLedgerProofJson(proof []merkle.LedgerNode) []any {
	if proof == nil {
		return nil
	}
	nodes := []any{}
	for _, node := range proof {
		nodes = append(nodes, map[string]any{"hash": "0x" + hex.EncodeToString(node.Hash[:]), "sum": node.Sum.String()})
	}
	return nodes
}

// testLedgerClaimEntry is a finalized account of the ledger answer.
func testLedgerClaimEntry(vault string, noId int64, index int64, rootIndex int64, cumulative int64, proof []merkle.LedgerNode, claimed int64) map[string]any {
	return map[string]any{
		"vault": vault, "no_id": noId, "index": index, "root_index": rootIndex,
		"cumulative": strconv.FormatInt(cumulative, 10), "proof": testLedgerProofJson(proof),
		"claimed": strconv.FormatInt(claimed, 10), "pending_cumulative": nil,
	}
}

func testLedgerClaimAnswer(t *testing.T, coldkey [32]byte, entries ...map[string]any) []byte {
	accounts := []any{}
	for _, entry := range entries {
		accounts = append(accounts, entry)
	}
	return testLedgerJson(t, map[string]any{"coldkey": "0x" + hex.EncodeToString(coldkey[:]), "accounts": accounts})
}

// The fixture: pool 1 of vault2 has two finalized roots. In root 1, of five
// accounts, alice is account 2 with 123,456,789 rao, of which vault2 has
// paid 100,000,000; in root 0 she had 110,000,000.
type testV2Claim struct {
	alice     [32]byte
	aliceSs58 string
	chain     *fakeVault2
	ledger    *testLedgerService
	settings  *SnChainSettings
	proofs    [][]merkle.LedgerNode
	oldProofs [][]merkle.LedgerNode
	key       *secp.PrivateKey
}

const (
	testV2Cumulative    = int64(123_456_789)
	testV2OldCumulative = int64(110_000_000)
	testV2Paid          = int64(100_000_000)
)

func newTestV2Claim(t *testing.T) *testV2Claim {
	t.Helper()
	alice, _ := testAliceSs58(t)
	return newTestV2ClaimFor(t, alice)
}

// newTestV2ClaimFor is the fixture with the wallet coldkey alice.
func newTestV2ClaimFor(t *testing.T, alice [32]byte) *testV2Claim {
	t.Helper()
	aliceSs58, err := ss58.Encode(alice, SnSs58Prefix)
	if err != nil {
		t.Fatal(err)
	}
	others := []testLedgerLeaf{{testColdkey(0x40), 5_000}, {testColdkey(0x50), 900_000_000}, {testColdkey(0x60), 7}, {testColdkey(0x70), 1}}
	oldRoot, oldTotal, oldProofs := buildTestLedgerTree([]testLedgerLeaf{others[0], others[1], {alice, testV2OldCumulative}})
	root, total, proofs := buildTestLedgerTree([]testLedgerLeaf{others[0], others[1], {alice, testV2Cumulative}, others[2], others[3]})
	chain := newFakeVault2(t)
	chain.roots[1] = []fakeVault2Root{{oldRoot, oldTotal, 3}, {root, total, 5}}
	chain.setClaimed(1, 2, testV2Paid)
	chainServer := httptest.NewServer(chain)
	t.Cleanup(chainServer.Close)

	ledger, ledgerServer := newTestLedgerService(t, http.StatusOK, testLedgerClaimAnswer(t, alice,
		testLedgerClaimEntry(testVault2, 1, 2, 1, testV2Cumulative, proofs[2], testV2Paid)))

	settings := SnTestnetChainSettings()
	settings.SetRpcUrls(nil)
	settings.AddRpcUrl(chainServer.URL)
	settings.Vault2Address = testVault2
	settings.ArtifactBaseUrl = ledgerServer.URL
	key, err := evm.PrivateKeyFromHex("0x4646464646464646464646464646464646464646464646464646464646464646")
	if err != nil {
		t.Fatal(err)
	}
	return &testV2Claim{alice: alice, aliceSs58: aliceSs58, chain: chain, ledger: ledger, settings: settings, proofs: proofs, oldProofs: oldProofs, key: key}
}

func (self *testV2Claim) engine(t *testing.T) *snLedgerClaimEngine {
	t.Helper()
	fetch := func(ctx context.Context, requestUrl string) ([]byte, error) {
		return snHttpGet(ctx, requestUrl, "the ledger account")
	}
	engine, snErr := newSnLedgerClaimEngine(self.settings, self.aliceSs58, "", fetch)
	if snErr != nil {
		t.Fatalf("engine: %+v", snErr)
	}
	return engine
}

// the calldata of alice's claim in root rootIndex
func (self *testV2Claim) calldata(t *testing.T, rootIndex int64, cumulative int64, proof []merkle.LedgerNode) []byte {
	t.Helper()
	data, err := evm.PackLedgerClaim(1, uint64(rootIndex), 2, self.alice, big.NewInt(cumulative), proof)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// answer sets the ledger's answer to these accounts of alice.
func (self *testV2Claim) answer(t *testing.T, entries ...map[string]any) {
	self.ledger.answer(http.StatusOK, testLedgerClaimAnswer(t, self.alice, entries...))
}

type testLedgerClaimEvent struct {
	kind    string
	noId    int64
	index   int64
	txHash  string
	amount  int64
	message string
}

// recordingLedgerClaimCallback records the progress of a claim in order.
type recordingLedgerClaimCallback struct {
	mutex  sync.Mutex
	events []testLedgerClaimEvent
	done   chan struct{}
}

func newRecordingLedgerClaimCallback() *recordingLedgerClaimCallback {
	return &recordingLedgerClaimCallback{done: make(chan struct{}, 1)}
}

func (self *recordingLedgerClaimCallback) add(event testLedgerClaimEvent) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.events = append(self.events, event)
}

func (self *recordingLedgerClaimCallback) Sent(noId int64, index int64, txHash string) {
	self.add(testLedgerClaimEvent{kind: "sent", noId: noId, index: index, txHash: txHash})
}

func (self *recordingLedgerClaimCallback) Confirmed(noId int64, index int64, txHash string, amountRao int64) {
	self.add(testLedgerClaimEvent{kind: "confirmed", noId: noId, index: index, txHash: txHash, amount: amountRao})
}

func (self *recordingLedgerClaimCallback) Failed(noId int64, index int64, message string) {
	self.add(testLedgerClaimEvent{kind: "failed", noId: noId, index: index, message: message})
}

func (self *recordingLedgerClaimCallback) Done() {
	self.add(testLedgerClaimEvent{kind: "done"})
	self.done <- struct{}{}
}

func (self *recordingLedgerClaimCallback) wait(t *testing.T) []testLedgerClaimEvent {
	t.Helper()
	select {
	case <-self.done:
	case <-time.After(60 * time.Second):
		t.Fatal("the claim did not finish")
	}
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return append([]testLedgerClaimEvent(nil), self.events...)
}

// claim runs one SnLedgerClaim of the engine's accounts.
func (self *testV2Claim) claim(t *testing.T) []testLedgerClaimEvent {
	t.Helper()
	callback := newRecordingLedgerClaimCallback()
	self.engine(t).claim(context.Background(), self.key, callback)
	callback.mutex.Lock()
	defer callback.mutex.Unlock()
	return append([]testLedgerClaimEvent(nil), callback.events...)
}

// requireFailedOnly requires the claim to have failed once with a message
// starting with prefix, with nothing sent.
func (self *testV2Claim) requireFailedOnly(t *testing.T, events []testLedgerClaimEvent, noId int64, index int64, prefix string) {
	t.Helper()
	if len(events) != 1 || events[0].kind != "failed" || events[0].noId != noId || events[0].index != index || !strings.HasPrefix(events[0].message, prefix) {
		t.Fatalf("events %+v, want one failure of %d/%d with %q", events, noId, index, prefix)
	}
	if sent := self.chain.sentTxs(); len(sent) != 0 {
		t.Fatalf("%d transactions sent", len(sent))
	}
}

func (self *testV2Claim) requireClaimable(t *testing.T) {
	t.Helper()
	if paid := self.chain.paid(1, 2); paid != testV2Paid {
		t.Fatalf("vault2 paid %d, want %d: the account is no longer claimable", paid, testV2Paid)
	}
}

// the maximum cost of the claim at the fixture's prices: the estimate plus a
// quarter, which the fixture's estimate puts above snLedgerClaimGasFloor, at
// 1.1 × eth_gasPrice
func testV2MaxCost(chain *fakeVault2) *big.Int {
	limit := chain.estimate + chain.estimate/4
	price := new(big.Int).Div(new(big.Int).Mul(chain.gasPrice, big.NewInt(11)), big.NewInt(10))
	return new(big.Int).Mul(price, new(big.Int).SetUint64(limit))
}

// APPS-SPEC claim rules 1, 2, 3 and 5: the account is verified against
// vault2, preflighted from the gas key, sent as one legacy EIP-155
// transaction to vault2 with value 0, a gas limit from eth_estimateGas and
// 1.1 × the gas price, and confirmed with the Claimed event's amount.
func TestSnLedgerClaimPaysTheClaimedAmount(t *testing.T) {
	f := newTestV2Claim(t)
	events := f.claim(t)
	sent := f.chain.sentTxs()
	if len(sent) != 1 || len(events) != 2 {
		t.Fatalf("events %+v, %d sent", events, len(sent))
	}
	tx := sent[0]
	wantPrice := big.NewInt(11_000_000_000)
	if tx.dynamic || tx.from != evm.AddressOf(f.key) || tx.to != f.chain.vault2 || tx.value.Sign() != 0 || tx.nonce != 5 ||
		tx.gas != 2_514_273+628_568 || tx.gasPrice.Cmp(wantPrice) != 0 || !bytes.Equal(tx.data, f.calldata(t, 1, testV2Cumulative, f.proofs[2])) {
		t.Fatalf("transaction %+v", tx)
	}
	want := []testLedgerClaimEvent{
		{kind: "sent", noId: 1, index: 2, txHash: events[0].txHash},
		{kind: "confirmed", noId: 1, index: 2, txHash: events[0].txHash, amount: testV2Cumulative - testV2Paid},
	}
	if !reflect.DeepEqual(events, want) || !strings.HasPrefix(events[0].txHash, "0x") || len(events[0].txHash) != 66 {
		t.Fatalf("events %+v, want %+v", events, want)
	}
	if paid := f.chain.paid(1, 2); paid != testV2Cumulative {
		t.Fatalf("vault2 paid %d", paid)
	}
	// rule 1, then rule 3: vault2's root and claimed in one batch, the
	// preflight from the gas key, the estimate, the balance, the nonce, the
	// price and the send
	wantMethods := []string{"eth_call", "eth_call", "eth_call", "eth_estimateGas", "eth_getBalance", "eth_getTransactionCount", "eth_gasPrice", "eth_sendRawTransaction", "eth_getTransactionReceipt"}
	if methods := f.chain.methods(); !reflect.DeepEqual(methods, wantMethods) {
		t.Fatalf("methods %q, want %q", methods, wantMethods)
	}
	gasKey := strings.ToLower(evm.AddressHex(evm.AddressOf(f.key)))
	if calls := f.chain.claimCalls(); len(calls) != 2 || calls[0].method != "eth_call" || calls[0].from != gasKey || calls[1].method != "eth_estimateGas" || calls[1].from != gasKey || calls[0].to != testVault2 {
		t.Fatalf("claim calls %+v", calls)
	}
}

// APPS-SPEC claim rule 5: Confirmed reports the amount vault2 paid, from its
// Claimed event, not the amount planned. A third party's claim with the older
// root paid 10,000,000 more between the preflight and the transaction.
func TestSnLedgerClaimConfirmsTheEventAmount(t *testing.T) {
	f := newTestV2Claim(t)
	f.chain.beforeMine = func() { f.chain.setClaimed(1, 2, testV2OldCumulative) }
	events := f.claim(t)
	if len(events) != 2 || events[1].kind != "confirmed" || events[1].amount != testV2Cumulative-testV2OldCumulative {
		t.Fatalf("events %+v", events)
	}

	// a receipt without vault2's Claimed event for the account confirms nothing
	f = newTestV2Claim(t)
	f.chain.dropClaimedLog = true
	events = f.claim(t)
	if len(events) != 2 || events[0].kind != "sent" || events[1].kind != "failed" || events[1].message != SnErrorCodeClaimFailed+": the claim transaction has no Claimed event for this account" {
		t.Fatalf("events %+v", events)
	}
}

// APPS-SPEC claim rule 3: an EIP-1559 claim, when the settings ask for one
// and the chain has a base fee, pays at most twice the base fee plus the tip.
func TestSnLedgerClaimSignsEip1559(t *testing.T) {
	f := newTestV2Claim(t)
	f.settings.TxType = SnTxTypeEip1559
	f.chain.baseFee = big.NewInt(4_000_000_000)
	events := f.claim(t)
	sent := f.chain.sentTxs()
	if len(sent) != 1 || len(events) != 2 || events[1].kind != "confirmed" {
		t.Fatalf("events %+v, %d sent", events, len(sent))
	}
	tx := sent[0]
	if !tx.dynamic || tx.from != evm.AddressOf(f.key) || tx.to != f.chain.vault2 || tx.value.Sign() != 0 || tx.gas != 2_514_273+628_568 ||
		tx.tip.Int64() != 2_000_000_000 || tx.feeCap.Int64() != 10_000_000_000 || !bytes.Equal(tx.data, f.calldata(t, 1, testV2Cumulative, f.proofs[2])) {
		t.Fatalf("transaction %+v", tx)
	}
}

// APPS-SPEC claim rule 1: a proof the ledger altered, a cumulative it
// inflated, or a root index vault2 does not have is proof_mismatch, and
// nothing is sent.
func TestSnLedgerClaimRefusesProofMismatch(t *testing.T) {
	f := newTestV2Claim(t)
	cases := []struct {
		name  string
		entry map[string]any
		want  string
	}{
		{"an inflated cumulative", testLedgerClaimEntry(testVault2, 1, 2, 1, testV2Cumulative+1, f.proofs[2], testV2Paid), SnErrorCodeProofMismatch + ": the ledger's proof does not verify against vault2's root"},
		{"a sibling sum lowered", testLedgerClaimEntry(testVault2, 1, 2, 1, testV2Cumulative, []merkle.LedgerNode{f.proofs[2][0], {Hash: f.proofs[2][1].Hash, Sum: big.NewInt(1)}, f.proofs[2][2]}, testV2Paid), SnErrorCodeProofMismatch + ": the ledger's proof does not verify against vault2's root"},
		{"another account's proof", testLedgerClaimEntry(testVault2, 1, 2, 1, testV2Cumulative, f.proofs[1], testV2Paid), SnErrorCodeProofMismatch + ": the ledger's proof does not verify against vault2's root"},
		{"the proof of the older root", testLedgerClaimEntry(testVault2, 1, 2, 1, testV2Cumulative, f.oldProofs[2], testV2Paid), SnErrorCodeProofMismatch + ": the ledger's proof does not verify against vault2's root"},
		{"another index", testLedgerClaimEntry(testVault2, 1, 3, 1, testV2Cumulative, f.proofs[2], testV2Paid), SnErrorCodeProofMismatch + ": the ledger's proof does not verify against vault2's root"},
		{"another pool", testLedgerClaimEntry(testVault2, 2, 2, 1, testV2Cumulative, f.proofs[2], testV2Paid), SnErrorCodeProofMismatch + ": vault2 has no root 1 in pool 2"},
		{"a root vault2 does not have", testLedgerClaimEntry(testVault2, 1, 2, 2, testV2Cumulative, f.proofs[2], testV2Paid), SnErrorCodeProofMismatch + ": vault2 has no root 2 in pool 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTestV2Claim(t)
			f.answer(t, c.entry)
			events := f.claim(t)
			noId, index := c.entry["no_id"].(int64), c.entry["index"].(int64)
			f.requireFailedOnly(t, events, noId, index, c.want)
			if events[0].message != c.want {
				t.Fatalf("message %q", events[0].message)
			}
			if calls := f.chain.claimCalls(); len(calls) != 0 {
				t.Fatalf("an unverified account was preflighted: %+v", calls)
			}
			f.requireClaimable(t)
			_, err := f.engine(t).unsignedTxs(context.Background())
			if err == nil || err.Error() != c.want {
				t.Fatalf("unsigned transactions: %v", err)
			}
		})
	}
}

// APPS-SPEC claim rule 1: an account the ledger lists in a vault other than
// the pinned Vault2Address is refused without a call to that vault.
func TestSnLedgerClaimRefusesAnotherVault(t *testing.T) {
	f := newTestV2Claim(t)
	other := "0x" + strings.Repeat("cd", 20)
	f.answer(t, testLedgerClaimEntry(other, 1, 2, 1, testV2Cumulative, f.proofs[2], testV2Paid))
	events := f.claim(t)
	want := SnErrorCodeNotClaimable + ": the ledger lists this account in " + other + ", not in the configured vault2 " + testVault2
	f.requireFailedOnly(t, events, 1, 2, want)
	if events[0].message != want || len(f.chain.methods()) != 0 {
		t.Fatalf("message %q, chain calls %q", events[0].message, f.chain.methods())
	}
	if _, err := f.engine(t).unsignedTxs(context.Background()); err == nil || err.Error() != want {
		t.Fatalf("unsigned transactions: %v", err)
	}

	// beside an account in vault2, which is claimed
	f.answer(t, testLedgerClaimEntry(other, 1, 2, 1, testV2Cumulative, f.proofs[2], testV2Paid), testLedgerClaimEntry(testVault2, 1, 2, 1, testV2Cumulative, f.proofs[2], testV2Paid))
	events = f.claim(t)
	if len(events) != 3 || events[0].kind != "failed" || events[1].kind != "sent" || events[2].kind != "confirmed" || len(f.chain.sentTxs()) != 1 {
		t.Fatalf("events %+v", events)
	}
}

// APPS-SPEC claim rules 1 and 2: Vault2Address pins the vault as an address,
// not as a spelling. A checksummed one pins the same vault2 as the ledger's
// lowercase vault, and the claim goes to its lowercase form.
func TestSnLedgerClaimChecksummedVault2Address(t *testing.T) {
	f := newTestV2Claim(t)
	f.settings.Vault2Address = "0x" + strings.ToUpper(strings.TrimPrefix(testVault2, "0x"))
	txs, err := SnLedgerClaimTransactionsFor(f.settings, f.aliceSs58)
	if err != nil || txs.Len() != 1 || txs.Get(0).To != testVault2 {
		t.Fatalf("transactions %+v, %v", txs, err)
	}
	events := f.claim(t)
	if sent := f.chain.sentTxs(); len(events) != 2 || events[1].kind != "confirmed" || len(sent) != 1 || sent[0].to != f.chain.vault2 {
		t.Fatalf("events %+v, sent %+v", events, sent)
	}
}

// APPS-SPEC claim rule 1: nothing is claimable when vault2 has paid the
// cumulative and the ledger says so, when the account is only in the pending
// root, and when the coldkey has no account: one not_claimable, nothing sent.
func TestSnLedgerClaimNothingClaimable(t *testing.T) {
	for _, c := range []struct {
		name    string
		claimed int64
		entries func(f *testV2Claim) []map[string]any
	}{
		{"paid in full", testV2Cumulative, func(f *testV2Claim) []map[string]any {
			return []map[string]any{testLedgerClaimEntry(testVault2, 1, 2, 1, testV2Cumulative, f.proofs[2], testV2Cumulative)}
		}},
		{"only pending", testV2Paid, func(f *testV2Claim) []map[string]any {
			return []map[string]any{{"vault": testVault2, "no_id": 1, "index": 2, "root_index": nil, "cumulative": "0", "proof": nil, "claimed": "0", "pending_cumulative": "130000000"}}
		}},
		{"no account", testV2Paid, func(f *testV2Claim) []map[string]any { return nil }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newTestV2Claim(t)
			f.chain.setClaimed(1, 2, c.claimed)
			f.answer(t, c.entries(f)...)
			events := f.claim(t)
			f.requireFailedOnly(t, events, 0, 0, snLedgerNothingToClaim)
			if calls := f.chain.claimCalls(); len(calls) != 0 {
				t.Fatalf("claim calls %+v", calls)
			}
			if _, err := f.engine(t).unsignedTxs(context.Background()); err == nil || err.Error() != snLedgerNothingToClaim {
				t.Fatalf("unsigned transactions: %v", err)
			}
		})
	}
}

// APPS-SPEC claim rule 1: a claim pays the finalized cumulative less vault2's
// claimed, read on chain; the ledger's claimed is only shown. A ledger that
// understates it, overstates it, or says the account is paid in full changes
// neither what is claimed nor its amount.
func TestSnLedgerClaimAmountIsCumulativeLessVault2Claimed(t *testing.T) {
	for name, ledgerClaimed := range map[string]int64{"understated": 0, "overstated": testV2Cumulative - 1, "paid in full": testV2Cumulative} {
		t.Run(name, func(t *testing.T) {
			f := newTestV2Claim(t)
			f.answer(t, testLedgerClaimEntry(testVault2, 1, 2, 1, testV2Cumulative, f.proofs[2], ledgerClaimed))
			txs, err := f.engine(t).unsignedTxs(context.Background())
			if err != nil || txs.Len() != 1 || txs.Get(0).AmountRao != testV2Cumulative-testV2Paid {
				t.Fatalf("transactions %+v, %v", txs, err)
			}
			events := f.claim(t)
			if len(events) != 2 || events[0].kind != "sent" || events[1].kind != "confirmed" || events[1].amount != testV2Cumulative-testV2Paid {
				t.Fatalf("events %+v", events)
			}
			if paid := f.chain.paid(1, 2); paid != testV2Cumulative {
				t.Fatalf("vault2 paid %d", paid)
			}
		})
	}
}

// APPS-SPEC claim rule 4 and SPEC A6: another claim paying the account first
// is already_claimed, success for the account, whether vault2 shows it when
// the claim is planned, refuses the preflight with NothingToClaim, or reverts
// the sent claim with it.
func TestSnLedgerClaimAfterAThirdPartyIsAlreadyClaimed(t *testing.T) {
	// planned: vault2 has paid what the ledger still shows as claimable
	f := newTestV2Claim(t)
	f.chain.setClaimed(1, 2, testV2Cumulative)
	events := f.claim(t)
	f.requireFailedOnly(t, events, 1, 2, SnErrorCodeAlreadyClaimed+": vault2 has paid this account its finalized cumulative")
	if _, err := f.engine(t).unsignedTxs(context.Background()); err == nil || !strings.HasPrefix(err.Error(), SnErrorCodeAlreadyClaimed+": ") {
		t.Fatalf("unsigned transactions: %v", err)
	}

	// the preflight
	f = newTestV2Claim(t)
	f.chain.beforeClaimCall = func() { f.chain.setClaimed(1, 2, testV2Cumulative) }
	events = f.claim(t)
	f.requireFailedOnly(t, events, 1, 2, SnErrorCodeAlreadyClaimed+": NothingToClaim: another claim paid this account first")
	if methods := f.chain.methods(); methods[len(methods)-1] != "eth_call" {
		t.Fatalf("methods after a refused preflight %q", methods)
	}

	// the sent claim
	f = newTestV2Claim(t)
	f.chain.beforeMine = func() { f.chain.setClaimed(1, 2, testV2Cumulative) }
	events = f.claim(t)
	if len(events) != 2 || events[0].kind != "sent" || events[1].kind != "failed" || events[1].noId != 1 || events[1].index != 2 ||
		events[1].message != SnErrorCodeAlreadyClaimed+": NothingToClaim: another claim paid this account first" {
		t.Fatalf("events %+v", events)
	}

	// the unsigned transaction's preflight
	f = newTestV2Claim(t)
	f.chain.beforeClaimCall = func() { f.chain.setClaimed(1, 2, testV2Cumulative) }
	if _, err := f.engine(t).unsignedTxs(context.Background()); err == nil || err.Error() != SnErrorCodeAlreadyClaimed+": NothingToClaim: another claim paid this account first" {
		t.Fatalf("unsigned transactions: %v", err)
	}
}

// APPS-SPEC claim rule 3: the gas key must hold the gas limit × the price;
// one wei less is needs_gas, with nothing sent.
func TestSnLedgerClaimNeedsGas(t *testing.T) {
	f := newTestV2Claim(t)
	maxCost := testV2MaxCost(f.chain)
	f.chain.balance = new(big.Int).Sub(maxCost, big.NewInt(1))
	events := f.claim(t)
	gasKey := snGasKeyInfo(f.key)
	want := snNeedsGasMessage(maxCost, f.chain.balance, gasKey)
	f.requireFailedOnly(t, events, 1, 2, SnErrorCodeNeedsGas+": this claim needs up to 0.034571 TAO of gas, the gas key "+gasKey.Address)
	if events[0].message != want || !strings.HasSuffix(want, "; send TAO to "+gasKey.MirrorSs58) {
		t.Fatalf("message %q, want %q", events[0].message, want)
	}
	f.requireClaimable(t)

	f = newTestV2Claim(t)
	f.chain.balance = maxCost
	if events := f.claim(t); len(events) != 2 || events[1].kind != "confirmed" {
		t.Fatalf("with exactly the limit × the price: %+v", events)
	}
}

// CL-4, APPS-SPEC claim rule 3: the gas key must hold the limit the claim is
// signed with × the price. Under a low estimate that is the cost of
// snLedgerClaimGasFloor: a gas key that covers the estimate with its quarter,
// or the floor less one wei, is needs_gas with nothing sent. At the plan's
// 5 gwei gas price a legacy claim, the default, then needs 0.0143 TAO, which
// the 0.02 TAO the plan asks for on the gas key covers (VERSION2-FINAL.md
// §11). An EIP-1559 claim is held to twice the base fee plus the tip, which
// 0.02 TAO does not cover.
func TestSnLedgerClaimNeedsGasAtTheFloor(t *testing.T) {
	const gwei = 1_000_000_000
	planGasKeyBalance := big.NewInt(20_000_000 * gwei)
	for _, c := range []struct {
		name   string
		txType string
		// the wei per gas the claim is signed at, and what needs_gas asks for
		price int64
		needs string
		// whether the plan's 0.02 TAO on the gas key pays for the claim
		planCovers bool
	}{
		{"legacy at 1.1 × the gas price", SnTxTypeLegacy, 5_500_000_000, "0.014300 TAO", true},
		{"eip1559 at twice the base fee plus the tip", SnTxTypeEip1559, 12_000_000_000, "0.031200 TAO", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fixture := func(balance *big.Int) *testV2Claim {
				f := newTestV2Claim(t)
				f.settings.TxType = c.txType
				// runtime 478's estimate of an account's first claim
				f.chain.estimate = 206_967
				f.chain.gasPrice = big.NewInt(5 * gwei)
				if c.txType == SnTxTypeEip1559 {
					f.chain.baseFee = big.NewInt(5 * gwei)
				}
				f.chain.balance = balance
				return f
			}
			floorCost := new(big.Int).Mul(big.NewInt(2_600_000), big.NewInt(c.price))
			// what the estimate with its quarter would cost
			estimateCost := new(big.Int).Mul(big.NewInt(258_708), big.NewInt(c.price))
			for _, balance := range []*big.Int{estimateCost, new(big.Int).Sub(floorCost, big.NewInt(1))} {
				f := fixture(balance)
				events := f.claim(t)
				gasKey := snGasKeyInfo(f.key)
				f.requireFailedOnly(t, events, 1, 2, SnErrorCodeNeedsGas+": this claim needs up to "+c.needs+" of gas, the gas key "+gasKey.Address)
				if want := snNeedsGasMessage(floorCost, balance, gasKey); events[0].message != want {
					t.Fatalf("with %s wei: message %q, want %q", balance, events[0].message, want)
				}
				f.requireClaimable(t)
			}

			f := fixture(floorCost)
			events := f.claim(t)
			sent := f.chain.sentTxs()
			if len(events) != 2 || events[1].kind != "confirmed" || len(sent) != 1 {
				t.Fatalf("with exactly the floor × the price: events %+v, %d sent", events, len(sent))
			}
			price := sent[0].gasPrice
			if sent[0].dynamic {
				price = sent[0].feeCap
			}
			if sent[0].gas != 2_600_000 || sent[0].dynamic != (c.txType == SnTxTypeEip1559) || price.Int64() != c.price {
				t.Fatalf("signed %+v, want 2,600,000 gas at %d wei", sent[0], c.price)
			}

			f = fixture(planGasKeyBalance)
			events = f.claim(t)
			if !c.planCovers {
				f.requireFailedOnly(t, events, 1, 2, SnErrorCodeNeedsGas+": this claim needs up to "+c.needs+" of gas, the gas key ")
			} else if len(events) != 2 || events[1].kind != "confirmed" {
				t.Fatalf("with the plan's 0.02 TAO: events %+v", events)
			}
		})
	}
}

// APPS-SPEC claim rule 4: a transfer the staking precompile refuses reverts
// vault2's claim without data; the runtime's reason is decoded by
// simulating the transfer from vault2, and the account stays claimable.
// TooManyStakingHotkeys is claim_failed and the transfer floor not_claimable.
func TestSnLedgerClaimDecodesRuntimeRefusals(t *testing.T) {
	for _, c := range []struct {
		refusal string
		want    string
	}{
		{"TooManyStakingHotkeys", SnErrorCodeClaimFailed + ": TooManyStakingHotkeys"},
		{"AmountTooLow", SnErrorCodeNotClaimable + ": AmountTooLow: the claim is below the runtime's transfer floor"},
	} {
		t.Run(c.refusal, func(t *testing.T) {
			// at the preflight
			f := newTestV2Claim(t)
			f.chain.runtimeRefusal = c.refusal
			events := f.claim(t)
			f.requireFailedOnly(t, events, 1, 2, c.want)
			if events[0].message != c.want {
				t.Fatalf("message %q", events[0].message)
			}
			f.requireClaimable(t)
			// the simulation is vault2's transfer of the amount to alice on the escrow hotkey
			wantSimulation := fakeVault2Call{method: "eth_call", from: testVault2, to: "0x0000000000000000000000000000000000000805", selector: "17ce5f62"}
			if calls := f.chain.calls; calls[len(calls)-1] != wantSimulation {
				t.Fatalf("last call %+v", calls[len(calls)-1])
			}
			if _, err := f.engine(t).unsignedTxs(context.Background()); err == nil || err.Error() != c.want {
				t.Fatalf("unsigned transactions: %v", err)
			}

			// in the sent claim
			f = newTestV2Claim(t)
			f.chain.beforeMine = func() { f.chain.runtimeRefusal = c.refusal }
			events = f.claim(t)
			if len(events) != 2 || events[0].kind != "sent" || events[1].kind != "failed" || events[1].message != c.want {
				t.Fatalf("events %+v", events)
			}
			f.requireClaimable(t)
		})
	}
}

// APPS-SPEC claim rule 4: vault2's own refusals are decoded by name, and the
// account stays claimable.
func TestSnLedgerClaimDecodesVaultRefusals(t *testing.T) {
	for refusal, want := range map[string]string{
		"PoolCapExceeded":           SnErrorCodeClaimFailed + ": PoolCapExceeded",
		"RuntimeAccountingMismatch": SnErrorCodeClaimFailed + ": RuntimeAccountingMismatch",
		"AmountTooLarge":            SnErrorCodeClaimFailed + ": AmountTooLarge",
		"InvalidColdkey":            SnErrorCodeClaimFailed + ": InvalidColdkey",
		"Underfunded":               SnErrorCodeClaimFailed + ": Underfunded",
		"InvalidProof":              SnErrorCodeProofMismatch + ": InvalidProof",
		"UnknownRoot":               SnErrorCodeProofMismatch + ": UnknownRoot",
	} {
		t.Run(refusal, func(t *testing.T) {
			f := newTestV2Claim(t)
			f.chain.vaultRefusal = refusal
			events := f.claim(t)
			f.requireFailedOnly(t, events, 1, 2, want)
			if events[0].message != want {
				t.Fatalf("message %q, want %q", events[0].message, want)
			}
			f.requireClaimable(t)
		})
	}
}

// APPS-SPEC claim rule 4: vault2 pays neither the zero coldkey nor its own
// (InvalidColdkey), which it checks before the proof. The account of such a
// wallet verifies here against vault2's root, and its preflight's refusal is
// claim_failed with the decoded name: nothing is estimated or sent, and no
// transaction is built for an external signer.
func TestSnLedgerClaimInvalidColdkey(t *testing.T) {
	self, err := snColdkeyBytes(EvmMirrorSs58(testVault2))
	if err != nil {
		t.Fatal(err)
	}
	want := SnErrorCodeClaimFailed + ": InvalidColdkey"
	for name, coldkey := range map[string][32]byte{"vault2's own coldkey": self, "the zero coldkey": {}} {
		t.Run(name, func(t *testing.T) {
			f := newTestV2ClaimFor(t, coldkey)
			events := f.claim(t)
			f.requireFailedOnly(t, events, 1, 2, want)
			if events[0].message != want {
				t.Fatalf("message %q, want %q", events[0].message, want)
			}
			if methods := f.chain.methods(); !reflect.DeepEqual(methods, []string{"eth_call", "eth_call", "eth_call"}) {
				t.Fatalf("methods %q: the refused preflight went on", methods)
			}
			f.requireClaimable(t)
			if _, err := f.engine(t).unsignedTxs(context.Background()); err == nil || err.Error() != want {
				t.Fatalf("unsigned transactions: %v", err)
			}
		})
	}
}

// APPS-SPEC claim rule 4: a revert without data whose reason cannot be read
// is claim_failed without one: the transfer simulation passes, the escrow
// hotkey cannot be read, or the node's message has another form, which is
// shown as it is. A sent claim that reverted but now passes says so, and a
// preflight answered with something other than data is claim_failed.
func TestSnLedgerClaimRefusalsWithoutAReason(t *testing.T) {
	transfer, escrow, claim := "17ce5f62", "164bf360", "b7a6b907"
	for _, c := range []struct {
		name  string
		setup func(f *testV2Claim)
		want  string
	}{
		{"the simulation passes", func(f *testV2Claim) {
			f.chain.runtimeRefusal = "Unseen"
			f.chain.bySelector[transfer] = fakeVault2Answer{result: "0x"}
		}, SnErrorCodeClaimFailed + ": the claim reverted without a reason"},
		{"the escrow hotkey cannot be read", func(f *testV2Claim) {
			f.chain.runtimeRefusal = "TooManyStakingHotkeys"
			f.chain.bySelector[escrow] = fakeVault2Answer{err: testRevert(nil)}
		}, SnErrorCodeClaimFailed + ": the claim reverted without a reason"},
		{"the escrow hotkey is not 32 bytes", func(f *testV2Claim) {
			f.chain.runtimeRefusal = "TooManyStakingHotkeys"
			f.chain.bySelector[escrow] = fakeVault2Answer{result: "0x01"}
		}, SnErrorCodeClaimFailed + ": the claim reverted without a reason"},
		{"another message", func(f *testV2Claim) {
			f.chain.runtimeRefusal = "Unseen"
			f.chain.bySelector[transfer] = fakeVault2Answer{err: &evm.RpcError{Code: -32603, Message: "evm error: OutOfFund"}}
		}, SnErrorCodeClaimFailed + ": evm error: OutOfFund"},
		{"a preflight answered with a number", func(f *testV2Claim) {
			f.chain.bySelector[claim] = fakeVault2Answer{result: 7}
		}, SnErrorCodeClaimFailed + ": data is not a string: 7"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newTestV2Claim(t)
			c.setup(f)
			events := f.claim(t)
			f.requireFailedOnly(t, events, 1, 2, c.want)
			if events[0].message != c.want {
				t.Fatalf("message %q, want %q", events[0].message, c.want)
			}
			f.requireClaimable(t)
		})
	}

	// refused when mined, passing when simulated again
	f := newTestV2Claim(t)
	f.chain.beforeMine = func() {
		f.chain.runtimeRefusal = "TooManyStakingHotkeys"
		f.chain.beforeClaimCall = func() { f.chain.runtimeRefusal = "" }
	}
	events := f.claim(t)
	if len(events) != 2 || events[0].kind != "sent" || events[1].kind != "failed" || events[1].message != SnErrorCodeClaimFailed+": the claim transaction reverted" {
		t.Fatalf("events %+v", events)
	}
	f.requireClaimable(t)
}

// CL-4, APPS-SPEC claim rule 3: the gas limit is eth_estimateGas's after a
// passing eth_call, plus a quarter, and at least snLedgerClaimGasFloor,
// 2,600,000: a claim is never signed below what it needed before runtime 478,
// however low the estimate. An estimate the node refuses sends nothing, and a
// refused preflight is never estimated.
func TestSnLedgerClaimGasLimitFromTheEstimate(t *testing.T) {
	for _, c := range []struct {
		name     string
		estimate uint64
		want     uint64
	}{
		// runtime 478's estimate of an account's first claim, 258,708 with
		// its quarter, while vault2's staking-hotkey list is empty
		{"a low estimate is signed at the floor", 206_967, 2_600_000},
		{"a quarter one gas short of the floor", 2_079_999, 2_600_000},
		{"a quarter that makes the floor", 2_080_000, 2_600_000},
		{"a quarter one gas above the floor", 2_080_001, 2_600_001},
		// the highest estimate of runtime 475 (R-7)
		{"a high estimate is unchanged", 2_574_029, 3_217_536},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newTestV2Claim(t)
			f.chain.estimate = c.estimate
			events := f.claim(t)
			if sent := f.chain.sentTxs(); len(events) != 2 || events[1].kind != "confirmed" || len(sent) != 1 || sent[0].gas != c.want {
				t.Fatalf("events %+v, sent %+v, want a gas limit of %d", events, sent, c.want)
			}
		})
	}

	f := newTestV2Claim(t)
	f.chain.failing["eth_estimateGas"] = &evm.RpcError{Code: -32000, Message: "gas required exceeds allowance (10000000)"}
	events := f.claim(t)
	f.requireFailedOnly(t, events, 1, 2, SnErrorCodeClaimFailed+": gas required exceeds allowance (10000000)")
	if methods := f.chain.methods(); !reflect.DeepEqual(methods, []string{"eth_call", "eth_call", "eth_call", "eth_estimateGas"}) {
		t.Fatalf("methods %q", methods)
	}

	f = newTestV2Claim(t)
	f.chain.vaultRefusal = "PoolCapExceeded"
	f.claim(t)
	if methods := f.chain.methods(); !reflect.DeepEqual(methods, []string{"eth_call", "eth_call", "eth_call"}) {
		t.Fatalf("a refused preflight went on: %q", methods)
	}

	// a zero gas price is priced at one wei
	f = newTestV2Claim(t)
	f.chain.gasPrice = big.NewInt(0)
	f.claim(t)
	if sent := f.chain.sentTxs(); len(sent) != 1 || sent[0].gasPrice.Int64() != 1 {
		t.Fatalf("sent %+v", sent)
	}
}

// APPS-SPEC claim rule 3: an EIP-1559 claim without the node's tip uses one
// gwei, and a chain without a base fee is sent a legacy transaction.
func TestSnLedgerClaimEip1559Fallbacks(t *testing.T) {
	f := newTestV2Claim(t)
	f.settings.TxType = SnTxTypeEip1559
	f.chain.baseFee = big.NewInt(4_000_000_000)
	f.chain.failing["eth_maxPriorityFeePerGas"] = &evm.RpcError{Code: -32601, Message: "method not found"}
	f.claim(t)
	if sent := f.chain.sentTxs(); len(sent) != 1 || !sent[0].dynamic || sent[0].tip.Int64() != 1_000_000_000 || sent[0].feeCap.Int64() != 9_000_000_000 {
		t.Fatalf("sent %+v", sent)
	}

	f = newTestV2Claim(t)
	f.settings.TxType = SnTxTypeEip1559
	f.claim(t)
	if sent := f.chain.sentTxs(); len(sent) != 1 || sent[0].dynamic || sent[0].gasPrice.Int64() != 11_000_000_000 {
		t.Fatalf("sent %+v", sent)
	}
}

// APPS-SPEC claim rules 1 and 3: an unreadable ledger or chain fails the
// claim before any account, a chain read failing for one account fails that
// account, and each failed read of the send fails it.
func TestSnLedgerClaimReadFailures(t *testing.T) {
	f := newTestV2Claim(t)
	f.ledger.answer(http.StatusInternalServerError, []byte("ledger down"))
	events := f.claim(t)
	f.requireFailedOnly(t, events, 0, 0, SnErrorCodeLedgerUnavailable+": http 500 fetching the ledger account")
	if _, err := f.engine(t).unsignedTxs(context.Background()); err == nil || err.Error() != SnErrorCodeLedgerUnavailable+": http 500 fetching the ledger account" {
		t.Fatalf("unsigned transactions: %v", err)
	}

	f = newTestV2Claim(t)
	f.settings.SetRpcUrls(nil)
	f.settings.AddRpcUrl("http://127.0.0.1:1")
	events = f.claim(t)
	f.requireFailedOnly(t, events, 0, 0, SnErrorCodeChainRpcUnreachable+": ")

	f = newTestV2Claim(t)
	f.chain.failing["eth_call"] = &evm.RpcError{Code: -32603, Message: "internal error"}
	events = f.claim(t)
	f.requireFailedOnly(t, events, 1, 2, SnErrorCodeChainRpcError+": rpc error -32603: internal error")

	// a roots or claimed answer that does not decode fails the account
	for selector, want := range map[string]string{
		"0fc3cec0": SnErrorCodeChainRpcError + ": roots return too short: 1 bytes",
		"a54ab457": SnErrorCodeChainRpcError + ": uint return too short: 1 bytes",
	} {
		f := newTestV2Claim(t)
		f.chain.bySelector[selector] = fakeVault2Answer{result: "0x01"}
		events := f.claim(t)
		f.requireFailedOnly(t, events, 1, 2, want)
		if events[0].message != want {
			t.Fatalf("message %q, want %q", events[0].message, want)
		}
		f.requireClaimable(t)
	}

	for method, want := range map[string]string{
		"eth_getBalance":          SnErrorCodeClaimFailed + ": balance unavailable",
		"eth_getTransactionCount": SnErrorCodeClaimFailed + ": nonce unavailable",
		"eth_gasPrice":            SnErrorCodeClaimFailed + ": price unavailable",
		"eth_sendRawTransaction":  SnErrorCodeNeedsGas + ": insufficient funds for gas * price + value",
	} {
		t.Run(method, func(t *testing.T) {
			f := newTestV2Claim(t)
			message := strings.TrimPrefix(strings.TrimPrefix(want, SnErrorCodeClaimFailed+": "), SnErrorCodeNeedsGas+": ")
			f.chain.failing[method] = &evm.RpcError{Code: -32000, Message: message}
			events := f.claim(t)
			if len(events) != 1 || events[0].kind != "failed" || events[0].message != want {
				t.Fatalf("events %+v, want %q", events, want)
			}
			f.requireClaimable(t)
		})
	}

	// the receipt cannot be read once the claim is sent
	f = newTestV2Claim(t)
	f.chain.failing["eth_getTransactionReceipt"] = &evm.RpcError{Code: -32000, Message: "receipt unavailable"}
	events = f.claim(t)
	if len(events) != 2 || events[0].kind != "sent" || events[1].kind != "failed" || events[1].message != SnErrorCodeClaimFailed+": rpc error -32000: receipt unavailable" {
		t.Fatalf("events %+v", events)
	}
}

// APPS-SPEC claim rule 1: vault2's roots and claimed come from a JSON-RPC
// endpoint the SDK does not trust either. An answer that is not the view's
// return fails the account with chain_rpc_error, and a root that holds no
// tree is proof_mismatch; nothing is preflighted or sent, and the account
// stays claimable.
func TestSnLedgerClaimRefusesMalformedVaultReads(t *testing.T) {
	roots, claimed := "0fc3cec0", "a54ab457"
	beyondUint64 := new(big.Int).Lsh(big.NewInt(1), 64)
	for _, c := range []struct {
		name  string
		setup func(f *testV2Claim)
		want  string
	}{
		{"a leaf count beyond uint64", func(f *testV2Claim) {
			root := f.chain.roots[1][1]
			f.chain.bySelector[roots] = fakeVault2Answer{result: testRootsHex(root.root, root.total, beyondUint64, big.NewInt(9))}
		}, SnErrorCodeChainRpcError + ": root leaf count or epoch overflows uint64"},
		{"an epoch beyond uint64", func(f *testV2Claim) {
			root := f.chain.roots[1][1]
			f.chain.bySelector[roots] = fakeVault2Answer{result: testRootsHex(root.root, root.total, big.NewInt(5), beyondUint64)}
		}, SnErrorCodeChainRpcError + ": root leaf count or epoch overflows uint64"},
		{"a roots answer that is not a string", func(f *testV2Claim) {
			f.chain.bySelector[roots] = fakeVault2Answer{result: 7}
		}, SnErrorCodeChainRpcError + ": data is not a string: 7"},
		{"a claimed answer that is not hex", func(f *testV2Claim) {
			f.chain.bySelector[claimed] = fakeVault2Answer{result: "0xzz"}
		}, SnErrorCodeChainRpcError + ": encoding/hex: invalid byte: U+007A 'z'"},
		{"a batch answer without the claimed", func(f *testV2Claim) {
			f.chain.omitFromBatch = claimed
		}, SnErrorCodeChainRpcError + ": missing batch response"},
		{"a root of no tree", func(f *testV2Claim) {
			f.chain.bySelector[roots] = fakeVault2Answer{result: "0x" + strings.Repeat("00", 5*32)}
		}, SnErrorCodeProofMismatch + ": the ledger's proof does not verify against vault2's root"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newTestV2Claim(t)
			c.setup(f)
			events := f.claim(t)
			f.requireFailedOnly(t, events, 1, 2, c.want)
			if events[0].message != c.want {
				t.Fatalf("message %q, want %q", events[0].message, c.want)
			}
			if calls := f.chain.claimCalls(); len(calls) != 0 {
				t.Fatalf("an account read from a malformed answer was preflighted: %+v", calls)
			}
			f.requireClaimable(t)
			if _, err := f.engine(t).unsignedTxs(context.Background()); err == nil || err.Error() != c.want {
				t.Fatalf("unsigned transactions: %v", err)
			}
		})
	}
}

// APPS-SPEC "Vault2Address": without it, or without the chain, v2 claims are
// chain_not_configured; a coldkey that is not a Bittensor address is refused.
func TestSnLedgerClaimNotConfigured(t *testing.T) {
	f := newTestV2Claim(t)
	for _, c := range []struct {
		name   string
		mutate func(settings *SnChainSettings) *SnChainSettings
		want   string
	}{
		{"no settings", func(*SnChainSettings) *SnChainSettings { return nil }, "nil chain settings"},
		{"no rpc", func(s *SnChainSettings) *SnChainSettings { s.ClearRpcUrls(); return s }, "no rpc urls"},
		{"no chain id", func(s *SnChainSettings) *SnChainSettings { s.ChainId = 0; return s }, "chain id not set"},
		{"no vault2", func(s *SnChainSettings) *SnChainSettings { s.Vault2Address = ""; return s }, "vault2 address: not set"},
		{"the zero vault2", func(s *SnChainSettings) *SnChainSettings { s.Vault2Address = "0x" + strings.Repeat("00", 20); return s }, "vault2 address: the zero address"},
		{"a malformed vault2", func(s *SnChainSettings) *SnChainSettings { s.Vault2Address = "0x1234"; return s }, "vault2 address: address must be 20 bytes, got 2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			settings := c.mutate(f.settings.Copy())
			want := SnErrorCodeChainNotConfigured + ": " + c.want
			if _, snErr := newSnLedgerClaimEngine(settings, f.aliceSs58, "", nil); snErr == nil || snErr.Code+": "+snErr.Message != want {
				t.Fatalf("engine: %+v, want %q", snErr, want)
			}
			if txs, err := SnLedgerClaimTransactionsFor(settings, f.aliceSs58); txs != nil || err == nil || err.Error() != want {
				t.Fatalf("transactions for: %v", err)
			}
		})
	}
	if _, snErr := newSnLedgerClaimEngine(f.settings, "not an address", "", nil); snErr == nil || snErr.Code != SnErrorCodeInvalidAddress {
		t.Fatalf("an invalid coldkey: %+v", snErr)
	}
	if len(f.chain.methods()) != 0 || len(f.ledger.takeRequests()) != 0 {
		t.Fatal("an unconfigured claim read the chain or the ledger")
	}
}

// addPool2 gives alice a second account, in pool 2 of vault2, the only
// account of a depth-0 tree, with nothing paid.
func (self *testV2Claim) addPool2(t *testing.T) map[string]any {
	t.Helper()
	root, total, proofs := buildTestLedgerTree([]testLedgerLeaf{{self.alice, 7_000_000_000}})
	self.chain.roots[2] = []fakeVault2Root{{root, total, 1}}
	if len(proofs[0]) != 0 {
		t.Fatal("a one-account tree has a proof")
	}
	return testLedgerClaimEntry(testVault2, 2, 0, 0, 7_000_000_000, proofs[0], 0)
}

// CL-4, APPS-SPEC claim rules 1 and 2: the claims for an external signer, one
// per account with something to claim, in the ledger's order: to vault2,
// value 0, the claim calldata, the chain id, the root index, the amount and
// the gas limit to sign with, snLedgerClaimGasFloor, 2,600,000: the signer
// does not size the claim by an estimate, which follows vault2's
// staking-hotkey list. Each is preflighted without a sender, and one the vault
// refuses is left out.
func TestSnLedgerClaimTransactionsForExternalSigners(t *testing.T) {
	f := newTestV2Claim(t)
	pool2 := f.addPool2(t)
	f.answer(t, testLedgerClaimEntry(testVault2, 1, 2, 1, testV2Cumulative, f.proofs[2], testV2Paid), pool2)
	txs, err := SnLedgerClaimTransactionsFor(f.settings, " "+f.aliceSs58+" ")
	if err != nil {
		t.Fatal(err)
	}
	pool2Data, _ := evm.PackLedgerClaim(2, 0, 0, f.alice, big.NewInt(7_000_000_000), []merkle.LedgerNode{})
	want := []*SnLedgerUnsignedTx{
		{NoId: 1, Index: 2, RootIndex: 1, ChainId: SnChainIdTestnet, To: testVault2, Data: "0x" + hex.EncodeToString(f.calldata(t, 1, testV2Cumulative, f.proofs[2])), Value: "0", AmountRao: testV2Cumulative - testV2Paid, Gas: 2_600_000},
		{NoId: 2, Index: 0, RootIndex: 0, ChainId: SnChainIdTestnet, To: testVault2, Data: "0x" + hex.EncodeToString(pool2Data), Value: "0", AmountRao: 7_000_000_000, Gas: 2_600_000},
	}
	if got := txs.getAll(); !reflect.DeepEqual(got, want) {
		gotJson, _ := json.Marshal(got)
		wantJson, _ := json.Marshal(want)
		t.Fatalf("transactions\n%s\nwant\n%s", gotJson, wantJson)
	}
	if calls := f.chain.claimCalls(); len(calls) != 2 || calls[0].from != "" || calls[1].from != "" || calls[0].method != "eth_call" || calls[1].method != "eth_call" {
		t.Fatalf("preflights %+v", calls)
	}
	if requests := f.ledger.takeRequests(); len(requests) != 1 || requests[0] != "GET /sn/ledger/account?coldkey="+f.aliceSs58+" " {
		t.Fatalf("ledger requests %q", requests)
	}
	if len(f.chain.sentTxs()) != 0 {
		t.Fatal("building the transactions sent one")
	}
	// the json the apps read
	raw, _ := json.Marshal(txs.Get(1))
	if string(raw) != `{"no_id":2,"index":0,"root_index":0,"chain_id":945,"to":"`+testVault2+`","data":"0x`+hex.EncodeToString(pool2Data)+`","value":"0","amount_rao":7000000000,"gas":2600000}` {
		t.Fatalf("json %s", raw)
	}

	// pool 2, first in the ledger, is paid by a third party at its preflight:
	// pool 1's claim alone
	f.answer(t, pool2, testLedgerClaimEntry(testVault2, 1, 2, 1, testV2Cumulative, f.proofs[2], testV2Paid))
	f.chain.beforeClaimCall = func() { f.chain.setClaimed(2, 0, 7_000_000_000) }
	txs, err = f.engine(t).unsignedTxs(context.Background())
	if err != nil || txs.Len() != 1 || txs.Get(0).NoId != 1 {
		t.Fatalf("transactions %+v, %v", txs, err)
	}
}

// APPS-SPEC SnLedgerClaim: two accounts are claimed in turn, each with its
// own transaction and nonce, then Done.
func TestSnLedgerClaimTwoPoolsInTurn(t *testing.T) {
	f := newTestV2Claim(t)
	f.answer(t, testLedgerClaimEntry(testVault2, 1, 2, 1, testV2Cumulative, f.proofs[2], testV2Paid), f.addPool2(t))
	events := f.claim(t)
	sent := f.chain.sentTxs()
	if len(events) != 4 || len(sent) != 2 {
		t.Fatalf("events %+v", events)
	}
	want := []testLedgerClaimEvent{
		{kind: "sent", noId: 1, index: 2, txHash: events[0].txHash},
		{kind: "confirmed", noId: 1, index: 2, txHash: events[0].txHash, amount: testV2Cumulative - testV2Paid},
		{kind: "sent", noId: 2, index: 0, txHash: events[2].txHash},
		{kind: "confirmed", noId: 2, index: 0, txHash: events[2].txHash, amount: 7_000_000_000},
	}
	if !reflect.DeepEqual(events, want) || sent[0].nonce != 5 || sent[1].nonce != 6 || events[0].txHash == events[2].txHash {
		t.Fatalf("events %+v, nonces %d %d", events, sent[0].nonce, sent[1].nonce)
	}
}

// APPS-SPEC SnLedgerClaim and SnLedgerClaimTransactions on a device: a
// connected wallet is required, the gas key needs local state, and with both
// the claim is signed with the device gas key and read from the device's
// settings and artifact host, then Done.
func TestSnLedgerClaimOnDevices(t *testing.T) {
	f := newTestV2Claim(t)
	space, localState := testingLogoutCauseSpace(t, f.settings.ArtifactBaseUrl)
	settings := f.settings.Copy()
	settings.ArtifactBaseUrl = ""
	if err := localState.SetSnChainSettings(settings); err != nil {
		t.Fatal(err)
	}
	ctx, api := newTestApiForURL(t, f.settings.ArtifactBaseUrl)
	device := &snDevice{ctx: ctx, networkSpace: space, api: func() *Api { return api }, clientId: connect.NewId(), state: newDeviceSn()}

	callback := newRecordingLedgerClaimCallback()
	device.SnLedgerClaim(callback)
	if events := callback.wait(t); len(events) != 2 || events[0].kind != "failed" || events[0].noId != 0 || !strings.HasPrefix(events[0].message, SnErrorCodeConnectWalletFirst+": ") || events[1].kind != "done" {
		t.Fatalf("without a wallet: %+v", events)
	}
	if _, err := device.SnLedgerClaimTransactions(); err == nil || !strings.HasPrefix(err.Error(), SnErrorCodeConnectWalletFirst+": ") {
		t.Fatalf("transactions without a wallet: %v", err)
	}

	device.setSnWallet(&SnWallet{ColdkeySs58: f.aliceSs58})
	txs, err := device.SnLedgerClaimTransactions()
	if err != nil || txs.Len() != 1 || txs.Get(0).AmountRao != testV2Cumulative-testV2Paid || txs.Get(0).Gas != 2_600_000 {
		t.Fatalf("transactions %+v, %v", txs, err)
	}
	callback = newRecordingLedgerClaimCallback()
	device.SnLedgerClaim(callback)
	events := callback.wait(t)
	if len(events) != 3 || events[0].kind != "sent" || events[1].kind != "confirmed" || events[1].amount != testV2Cumulative-testV2Paid || events[2].kind != "done" {
		t.Fatalf("events %+v", events)
	}
	gasKey, _ := evm.ParseAddress(device.GetSnGasKey().Address)
	if sent := f.chain.sentTxs(); len(sent) != 1 || sent[0].from != gasKey {
		t.Fatalf("sent %+v, want from the device gas key %x", sent, gasKey)
	}
	if requests := f.ledger.takeRequests(); len(requests) != 2 {
		t.Fatalf("ledger requests %q", requests)
	}

	// settings with vault2 but no local state for the gas key
	bare := &snDevice{ctx: ctx, networkSpace: &NetworkSpace{values: NetworkSpaceValues{SnChain: f.settings}}, api: func() *Api { return api }, clientId: connect.NewId(), state: newDeviceSn()}
	bare.setSnWallet(&SnWallet{ColdkeySs58: f.aliceSs58})
	callback = newRecordingLedgerClaimCallback()
	bare.SnLedgerClaim(callback)
	if events := callback.wait(t); len(events) != 2 || events[0].message != SnErrorCodeLocalState+": "+SnErrorCodeLocalState || events[1].kind != "done" {
		t.Fatalf("without local state: %+v", events)
	}
}

// APPS-SPEC SnLedgerClaim on both devices: DeviceLocal asks for a wallet, and
// the app-side DeviceRemote claims while its tunnel process is down.
func TestSnLedgerClaimOnBothDevices(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	deviceSettings := DefaultDeviceLocalSettings()
	deviceSettings.DisableLogging = true
	deviceSettings.Verbose = false
	deviceLocal, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), deviceSettings, connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	defer deviceLocal.Close()
	callback := newRecordingLedgerClaimCallback()
	deviceLocal.SnLedgerClaim(callback)
	if events := callback.wait(t); len(events) != 2 || !strings.HasPrefix(events[0].message, SnErrorCodeConnectWalletFirst+": ") {
		t.Fatalf("device local without a wallet: %+v", events)
	}
	if _, err := deviceLocal.SnLedgerClaimTransactions(); err == nil || !strings.HasPrefix(err.Error(), SnErrorCodeConnectWalletFirst+": ") {
		t.Fatalf("device local transactions without a wallet: %v", err)
	}

	f := newTestV2Claim(t)
	server := httptest.NewServer(testApiHandler(f.ledger))
	defer server.Close()
	space, localState := testingLogoutCauseSpace(t, server.URL)
	settings := f.settings.Copy()
	settings.ArtifactBaseUrl = ""
	if err := localState.SetSnChainSettings(settings); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetSnWallet(&SnWallet{ColdkeySs58: f.aliceSs58}); err != nil {
		t.Fatal(err)
	}
	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, space, "")
	if txs, err := deviceRemote.SnLedgerClaimTransactions(); err != nil || txs.Len() != 1 {
		t.Fatalf("device remote transactions %+v, %v", txs, err)
	}
	callback = newRecordingLedgerClaimCallback()
	deviceRemote.SnLedgerClaim(callback)
	if events := callback.wait(t); len(events) != 3 || events[1].kind != "confirmed" || events[1].amount != testV2Cumulative-testV2Paid {
		t.Fatalf("device remote: %+v", events)
	}
}

// APPS-SPEC "Vault2Address": in the settings' json beside vault_address,
// overlaid by Merge, empty in the compiled mainnet and testnet defaults, and
// read from the network space's sn_chain values.
func TestSnChainSettingsVault2Address(t *testing.T) {
	if DefaultSnChainSettings().Vault2Address != "" || SnTestnetChainSettings().Vault2Address != "" {
		t.Fatal("a compiled default configures v2 claims before activation")
	}
	settings := SnTestnetChainSettings()
	settings.Vault2Address = testVault2
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"vault_address":"","vault2_address":"`+testVault2+`"`) {
		t.Fatalf("json %s", raw)
	}
	var back SnChainSettings
	if err := json.Unmarshal(raw, &back); err != nil || back.Vault2Address != testVault2 {
		t.Fatalf("round trip %+v, %v", back, err)
	}
	merged := DefaultSnChainSettings().Merge(settings)
	if merged.Vault2Address != testVault2 {
		t.Fatalf("merged %+v", merged)
	}
	if kept := merged.Merge(NewSnChainSettings()); kept.Vault2Address != testVault2 {
		t.Fatal("an empty override cleared vault2")
	}

	var values NetworkSpaceValues
	if err := json.Unmarshal([]byte(`{"sn_chain":{"vault2_address":"`+testVault2+`"}}`), &values); err != nil {
		t.Fatal(err)
	}
	device := &snDevice{networkSpace: &NetworkSpace{values: values}, state: newDeviceSn()}
	if got := device.GetSnChainSettings(); got.Vault2Address != testVault2 || got.ChainId != SnChainIdMainnet {
		t.Fatalf("network space settings %+v", got)
	}
}
