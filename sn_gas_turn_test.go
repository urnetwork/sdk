package sdk

// A gas key's claims take turns at its nonce (sn_gas.go). On one device, a v1
// claim (SnClaim) and a v2 claim (SnLedgerClaim) started together, two of
// either kind started together (a second window), a v2 batch, or the smaller
// batch a short balance sends, beside a v1 claim, and the claims of two devices
// sharing the gas key each send with the nonces n and n + 1, on a chain whose
// transaction pool checks nonces as a node's does, also when its nonce reads
// come from a node that has not seen the first transaction yet. The turn ends
// at the broadcast, so neither claim waits for the other's receipt, and every
// way a claim ends gives it back. A transaction holds its nonce only while its
// claim waits for the receipt, a refused one never; and a claim whose time
// runs out while it waits for the turn sends nothing.

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
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk/sn/evm"
)

// the v1 fixture's contracts (newTestV1Claim's)
const (
	testV1Vault       = "0x2222222222222222222222222222222222222222"
	testV1Coordinator = "0x1111111111111111111111111111111111111111"
)

// fakeNoncePool is a node's transaction pool in front of the v1 fake
// (fakeChain) and the v2 fake (fakeVault2), with one nonce for each sender.
// eth_getTransactionCount(pending) is the sender's mined transactions and those
// the pool holds next in a row. eth_sendRawTransaction refuses a transaction
// whose nonce is mined ("nonce too low"), or held for another transaction
// ("replacement transaction underpriced": the SDK never prices a claim up), or
// beyond the next one ("nonce too high": a node would hold it until the gap
// filled, which no test waits for), and otherwise holds it. A held transaction
// is mined when its receipt is first read, after its sender's lower nonces, by
// the fake of the contract it calls. Every other call goes to the fake of the
// contract it names.
type fakeNoncePool struct {
	t       *testing.T
	mutex   sync.Mutex
	v1      *fakeChain
	v2      *fakeVault2
	chainId *big.Int
	// the nonce each sender starts at, as the v2 fixture's
	firstNonce uint64
	mined      map[[20]byte]uint64
	held       map[[20]byte]map[uint64]*fakePoolTx
	byHash     map[string]*fakePoolTx
	landed     []*fakePoolTx
	refused    []string
	// the first nonce read waits up to pairWait for a second, and both answer
	// the pool's count when the second came, as two claims reading their
	// nonces at once do on a node; 0 waits for none
	pairWait time.Duration
	paired   bool
	waiting  chan uint64
	// no receipt is answered until this many transactions are held or mined
	receiptsAfter int
	// each broadcast takes this long to reach the pool, and a nonce read
	// meanwhile does not count it
	broadcastDelay time.Duration
	// run once, as the first nonce read comes, before it is answered
	atFirstNonceRead func()
	// nonce reads come from another node than broadcasts, which counts a held
	// transaction only this long after the pool took it (gossip)
	readLag time.Duration
	// the next refuseSends broadcasts are refused with refuseMessage
	refuseSends   int
	refuseMessage string
	// nonce reads fail
	nonceReadsFail bool
}

type fakePoolTx struct {
	tx   *fakeVault2Tx
	raw  []byte
	hash string
	// when the pool took it
	heldAt time.Time
	// the fake that mined it, nil while it is held
	minedBy http.Handler
}

type fakePoolRequest struct {
	Id     json.RawMessage   `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

func newFakeNoncePool(t *testing.T, v1 *fakeChain, v2 *fakeVault2) *fakeNoncePool {
	return &fakeNoncePool{
		t:          t,
		v1:         v1,
		v2:         v2,
		chainId:    big.NewInt(SnChainIdTestnet),
		firstNonce: 5,
		mined:      map[[20]byte]uint64{},
		held:       map[[20]byte]map[uint64]*fakePoolTx{},
		byHash:     map[string]*fakePoolTx{},
	}
}

func (self *fakeNoncePool) minedOf(from [20]byte) uint64 {
	if n, ok := self.mined[from]; ok {
		return n
	}
	return self.firstNonce
}

func (self *fakeNoncePool) pending(from [20]byte) uint64 {
	n := self.minedOf(from)
	for self.held[from][n] != nil {
		n++
	}
	return n
}

// pendingAsRead is pending as a nonce read answers it, on the node that
// counts a held transaction readLag after the pool took it.
func (self *fakeNoncePool) pendingAsRead(from [20]byte) uint64 {
	n := self.minedOf(from)
	for held := self.held[from][n]; held != nil && !time.Now().Before(held.heldAt.Add(self.readLag)); held = self.held[from][n] {
		n++
	}
	return n
}

// the fake of the contract `to`: v1's vault and coordinator, else v2's
func (self *fakeNoncePool) fakeOf(to [20]byte) http.Handler {
	if to == self.v1.vault || to == self.v1.coordinator {
		return self.v1
	}
	return self.v2
}

// the fake a call goes to: by the contract it names, else by its method
func (self *fakeNoncePool) target(req fakePoolRequest) http.Handler {
	switch req.Method {
	case "eth_call", "eth_estimateGas":
		var msg struct {
			To string `json:"to"`
		}
		if len(req.Params) > 0 && json.Unmarshal(req.Params[0], &msg) == nil {
			if to, err := evm.ParseAddress(msg.To); err == nil {
				return self.fakeOf(to)
			}
		}
	case "eth_blockNumber", "eth_chainId":
		return self.v1
	}
	return self.v2
}

func (self *fakeNoncePool) forward(handler http.Handler, body []byte) []byte {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)))
	return recorder.Body.Bytes()
}

func (self *fakeNoncePool) answer(w http.ResponseWriter, id json.RawMessage, result any, rpcErr *evm.RpcError) {
	res := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		res["error"] = map[string]any{"code": rpcErr.Code, "message": rpcErr.Message}
	} else {
		res["result"] = result
	}
	json.NewEncoder(w).Encode(res)
}

// nonceRead answers eth_getTransactionCount(from, pending).
func (self *fakeNoncePool) nonceRead(from [20]byte) uint64 {
	self.mutex.Lock()
	hook := self.atFirstNonceRead
	self.atFirstNonceRead = nil
	self.mutex.Unlock()
	if hook != nil {
		hook()
	}
	self.mutex.Lock()
	if self.waiting != nil {
		// the second read: both answer the pool as it is now
		pending := self.pendingAsRead(from)
		self.waiting <- pending
		self.waiting = nil
		self.mutex.Unlock()
		return pending
	}
	if self.pairWait == 0 || self.paired {
		defer self.mutex.Unlock()
		return self.pendingAsRead(from)
	}
	self.paired = true
	waiting := make(chan uint64, 1)
	self.waiting = waiting
	self.mutex.Unlock()
	select {
	case pending := <-waiting:
		return pending
	case <-time.After(self.pairWait):
	}
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.waiting == waiting {
		// no second read came
		self.waiting = nil
		return self.pendingAsRead(from)
	}
	// one came as the wait ended
	return <-waiting
}

func (self *fakeNoncePool) send(params []json.RawMessage) (any, *evm.RpcError) {
	var rawHex string
	json.Unmarshal(params[0], &rawHex)
	raw, err := evm.ParseHexBytes(rawHex)
	if err != nil {
		return nil, &evm.RpcError{Code: -32602, Message: err.Error()}
	}
	tx, err := decodeTestTx(raw, self.chainId)
	if err != nil {
		self.t.Errorf("sent transaction: %v", err)
		return nil, &evm.RpcError{Code: -32000, Message: err.Error()}
	}
	hash := evm.Keccak256(raw)
	txHash := "0x" + hex.EncodeToString(hash[:])
	refuse := func(message string) (any, *evm.RpcError) {
		self.refused = append(self.refused, fmt.Sprintf("nonce %d to %x: %s", tx.nonce, tx.to, message))
		return nil, &evm.RpcError{Code: -32000, Message: message}
	}
	switch {
	case 0 < self.refuseSends:
		self.refuseSends--
		return refuse(self.refuseMessage)
	case self.byHash[txHash] != nil:
		return refuse("already known")
	case tx.nonce < self.minedOf(tx.from):
		return refuse("nonce too low")
	case self.held[tx.from][tx.nonce] != nil:
		return refuse("replacement transaction underpriced")
	case self.pending(tx.from) < tx.nonce:
		return refuse("nonce too high")
	}
	if self.held[tx.from] == nil {
		self.held[tx.from] = map[uint64]*fakePoolTx{}
	}
	held := &fakePoolTx{tx: tx, raw: raw, hash: txHash, heldAt: time.Now()}
	self.held[tx.from][tx.nonce] = held
	self.byHash[txHash] = held
	return txHash, nil
}

// mine runs the held transaction through the fake of the contract it calls.
func (self *fakeNoncePool) mine(held *fakePoolTx) {
	held.minedBy = self.fakeOf(held.tx.to)
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 0, "method": "eth_sendRawTransaction", "params": []string{"0x" + hex.EncodeToString(held.raw)}})
	var res struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(self.forward(held.minedBy, body), &res); err != nil || res.Result != held.hash {
		self.t.Errorf("mining nonce %d: %q, %v", held.tx.nonce, res.Result, err)
	}
	self.landed = append(self.landed, held)
}

// receipt answers eth_getTransactionReceipt, mining the transaction first
func (self *fakeNoncePool) receipt(w http.ResponseWriter, req fakePoolRequest, body []byte) {
	var txHash string
	json.Unmarshal(req.Params[0], &txHash)
	held := self.byHash[strings.ToLower(txHash)]
	if held == nil || len(self.byHash) < self.receiptsAfter {
		self.answer(w, req.Id, nil, nil)
		return
	}
	from := held.tx.from
	for held.minedBy == nil {
		next := self.held[from][self.minedOf(from)]
		if next == nil {
			// a gap below it: never, with the refusals of send
			self.answer(w, req.Id, nil, nil)
			return
		}
		self.mine(next)
		delete(self.held[from], next.tx.nonce)
		self.mined[from] = next.tx.nonce + 1
	}
	w.Write(self.forward(held.minedBy, body))
}

func (self *fakeNoncePool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := readAllBody(r)
	w.Header().Set("Content-Type", "application/json")
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("[")) {
		// a batch of eth_calls, to the contract the first names
		var reqs []fakePoolRequest
		json.Unmarshal(body, &reqs)
		var target http.Handler = self.v2
		if len(reqs) > 0 {
			target = self.target(reqs[0])
		}
		self.mutex.Lock()
		defer self.mutex.Unlock()
		w.Write(self.forward(target, body))
		return
	}
	var req fakePoolRequest
	json.Unmarshal(body, &req)
	switch req.Method {
	case "eth_getTransactionCount":
		self.mutex.Lock()
		fail := self.nonceReadsFail
		self.mutex.Unlock()
		if fail {
			self.answer(w, req.Id, nil, &evm.RpcError{Code: -32000, Message: "nonce unavailable"})
			return
		}
		var address string
		json.Unmarshal(req.Params[0], &address)
		from, err := evm.ParseAddress(address)
		if err != nil {
			self.answer(w, req.Id, nil, &evm.RpcError{Code: -32602, Message: err.Error()})
			return
		}
		self.answer(w, req.Id, evm.HexQuantity(new(big.Int).SetUint64(self.nonceRead(from))), nil)
	case "eth_sendRawTransaction":
		self.mutex.Lock()
		delay := self.broadcastDelay
		self.mutex.Unlock()
		time.Sleep(delay)
		self.mutex.Lock()
		defer self.mutex.Unlock()
		result, rpcErr := self.send(req.Params)
		self.answer(w, req.Id, result, rpcErr)
	case "eth_getTransactionReceipt":
		self.mutex.Lock()
		defer self.mutex.Unlock()
		self.receipt(w, req, body)
	default:
		self.mutex.Lock()
		defer self.mutex.Unlock()
		w.Write(self.forward(self.target(req), body))
	}
}

// pairNonceReads makes the first nonce read wait up to wait for a second.
func (self *fakeNoncePool) pairNonceReads(wait time.Duration) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.pairWait = wait
}

// lagReads makes nonce reads count a held transaction only lag after the pool
// took it.
func (self *fakeNoncePool) lagReads(lag time.Duration) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.readLag = lag
}

// refuse refuses the next count broadcasts with message.
func (self *fakeNoncePool) refuse(count int, message string) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.refuseSends, self.refuseMessage = count, message
}

// failNonceReads makes nonce reads fail, or answer again.
func (self *fakeNoncePool) failNonceReads(fail bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.nonceReadsFail = fail
}

// drop drops the held transactions from `from`, as a pool drops one it will
// never mine, and returns how many it dropped.
func (self *fakeNoncePool) drop(from [20]byte) int {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	dropped := 0
	for nonce, held := range self.held[from] {
		delete(self.byHash, held.hash)
		delete(self.held[from], nonce)
		dropped++
	}
	return dropped
}

// startAtFirstNonceRead runs start as the first nonce read comes.
func (self *fakeNoncePool) startAtFirstNonceRead(start func()) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.atFirstNonceRead = start
}

// delayBroadcasts makes each broadcast take delay to reach the pool.
func (self *fakeNoncePool) delayBroadcasts(delay time.Duration) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.broadcastDelay = delay
}

// holdReceipts answers no receipt until count transactions are held or mined.
func (self *fakeNoncePool) holdReceipts(count int) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.receiptsAfter = count
}

// the transactions the pool held or mined, from every sender
func (self *fakeNoncePool) sentCount() int {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return len(self.byHash)
}

// the transactions mined from `from`, in order, and the node's refusals
func (self *fakeNoncePool) results(from [20]byte) ([]*fakePoolTx, []string) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	var landed []*fakePoolTx
	for _, held := range self.landed {
		if held.tx.from == from {
			landed = append(landed, held)
		}
	}
	return landed, append([]string(nil), self.refused...)
}

// recordingEpochClaimCallback records an SnClaim's progress in order, as
// recordingLedgerClaimCallback records an SnLedgerClaim's, with the epoch as
// the index.
type recordingEpochClaimCallback struct {
	recordingLedgerClaimCallback
}

// the events so far
func (self *recordingLedgerClaimCallback) recorded() []testLedgerClaimEvent {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return append([]testLedgerClaimEvent(nil), self.events...)
}

func newRecordingEpochClaimCallback() *recordingEpochClaimCallback {
	return &recordingEpochClaimCallback{recordingLedgerClaimCallback{done: make(chan struct{}, 1)}}
}

func (self *recordingEpochClaimCallback) Sent(epoch int64, txHash string) {
	self.add(testLedgerClaimEvent{kind: "sent", index: epoch, txHash: txHash})
}

func (self *recordingEpochClaimCallback) Confirmed(epoch int64, txHash string, amountRao int64) {
	self.add(testLedgerClaimEvent{kind: "confirmed", index: epoch, txHash: txHash, amount: amountRao})
}

func (self *recordingEpochClaimCallback) Failed(epoch int64, message string) {
	self.add(testLedgerClaimEvent{kind: "failed", index: epoch, message: message})
}

// alice's share of v1 epoch 7: 1,234 basis points of 1 alpha
const testV1Epoch7Rao = int64(123_400_000)

// testGasKeyClaims is one device whose gas key claims both ways: alice's v1
// share of epoch 7 (newTestV1Claim's) and her settlement v2 accounts (the v2
// fixture's), with every chain call through one fakeNoncePool and the v1
// artifact and the v2 ledger on the network space's api host.
type testGasKeyClaims struct {
	v2     *testV2Claim
	v1     *fakeChain
	pool   *fakeNoncePool
	device *snDevice
	gasKey [20]byte
	// the device's network space and api, for another device on them
	space *NetworkSpace
	api   *Api
}

func newTestGasKeyClaims(t *testing.T, v2 *testV2Claim) *testGasKeyClaims {
	t.Helper()
	raw7, hash7, root7 := buildTestArtifact(t, 7, 3, [][32]byte{v2.alice, testColdkey(0x40)}, []uint64{1234, 8766})
	vault, _ := evm.ParseAddress(testV1Vault)
	coordinator, _ := evm.ParseAddress(testV1Coordinator)
	v1 := &fakeChain{
		t: t, block: 1000, currentEpoch: 9, vault: vault, coordinator: coordinator,
		entitlements: map[int64]*evm.Entitlement{
			7: {PayoutRoot: root7, ArtifactHash: hash7, Funded: big.NewInt(1_000_000_000), Total: big.NewInt(1_000_000_000), Claimed: big.NewInt(0), ExpiryBlock: 5000, Status: evm.StatusFinalized},
		},
		claimed:  map[string]bool{},
		receipts: map[string]bool{},
	}
	pool := newFakeNoncePool(t, v1, v2.chain)
	chainServer := httptest.NewServer(pool)
	t.Cleanup(chainServer.Close)
	host := httptest.NewServer(testApiHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sn/artifact" {
			v2.ledger.ServeHTTP(w, r)
			return
		}
		if strings.TrimPrefix(r.URL.Query().Get("hash"), "sha256:") != hex.EncodeToString(hash7[:]) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw7)
	})))
	t.Cleanup(host.Close)

	settings := v2.settings.Copy()
	settings.SetRpcUrls(nil)
	settings.AddRpcUrl(chainServer.URL)
	settings.VaultAddress = testV1Vault
	settings.CoordinatorAddress = testV1Coordinator
	settings.NoId = "3"
	settings.ArtifactBaseUrl = ""
	space, localState := testingLogoutCauseSpace(t, host.URL)
	if err := localState.SetSnChainSettings(settings); err != nil {
		t.Fatal(err)
	}
	ctx, api := newTestApiForURL(t, host.URL)
	device := &snDevice{ctx: ctx, networkSpace: space, api: func() *Api { return api }, clientId: connect.NewId(), state: newDeviceSn()}
	device.setSnWallet(&SnWallet{ColdkeySs58: v2.aliceSs58})
	gasKey, err := evm.ParseAddress(device.GetSnGasKey().Address)
	if err != nil {
		t.Fatal(err)
	}
	return &testGasKeyClaims{v2: v2, v1: v1, pool: pool, device: device, gasKey: gasKey, space: space, api: api}
}

// anotherDevice is a device of its own on the same network space, so with the
// same local state and gas key, as a DeviceLocal and a DeviceRemote in one
// process are.
func (self *testGasKeyClaims) anotherDevice() *snDevice {
	return &snDevice{ctx: self.device.ctx, networkSpace: self.space, api: func() *Api { return self.api }, clientId: connect.NewId(), state: newDeviceSn()}
}

// requireTurnFree requires the gas key's turn to be free and nothing of the
// key's to be in flight.
func (self *testGasKeyClaims) requireTurnFree(t *testing.T, after string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	turn, err := snGasKeyTurns.take(ctx, self.gasKey)
	if err != nil {
		t.Fatalf("after %s the gas key's turn was still taken: %v", after, err)
	}
	turn.release()
	if floor := snGasKeyTurns.floor(self.gasKey); floor != 0 {
		t.Fatalf("after %s the nonce %d is still in flight", after, floor-1)
	}
}

// claimBoth starts a v1 claim of epoch 7 and a v2 claim on the device, the
// one v1First names first and the other as the first reads its nonce: without
// the turn, before the first's broadcast, and with it, while the first holds
// the turn. It waits for both Dones.
func (self *testGasKeyClaims) claimBoth(t *testing.T, v1First bool) ([]testLedgerClaimEvent, []testLedgerClaimEvent) {
	t.Helper()
	epochs := NewInt64List()
	epochs.Add(7)
	v1 := newRecordingEpochClaimCallback()
	v2 := newRecordingLedgerClaimCallback()
	first := func() { self.device.SnClaim(epochs, v1) }
	second := func() { self.device.SnLedgerClaim(v2) }
	if !v1First {
		first, second = second, first
	}
	self.pool.startAtFirstNonceRead(second)
	first()
	return v1.wait(t), v2.wait(t)
}

// the order of claimBoth
var testClaimOrders = []struct {
	name    string
	v1First bool
}{{"the v1 claim first", true}, {"the v2 claim first", false}}

// requireLanded requires the gas key's transactions to have landed with the
// nonces 5, 6, … in turn, none refused, and returns them.
func (self *testGasKeyClaims) requireLanded(t *testing.T, count int) []*fakePoolTx {
	t.Helper()
	landed, refused := self.pool.results(self.gasKey)
	var nonces []uint64
	for _, held := range landed {
		nonces = append(nonces, held.tx.nonce)
	}
	var want []uint64
	for i := 0; i < count; i++ {
		want = append(want, 5+uint64(i))
	}
	if len(refused) != 0 || !reflect.DeepEqual(nonces, want) {
		t.Fatalf("the gas key's transactions landed with the nonces %v, want %v; the node refused %q", nonces, want, refused)
	}
	return landed
}

// the landed transaction to `to`
func landedTo(t *testing.T, landed []*fakePoolTx, to [20]byte) *fakePoolTx {
	t.Helper()
	for _, held := range landed {
		if held.tx.to == to {
			return held
		}
	}
	t.Fatalf("no transaction to %x landed", to)
	return nil
}

// A v1 claim and a v2 claim started together on one device read the gas key's
// nonce together without the turn (the pool answers both 5), and one
// transaction is refused. With it each reads its own, the second claim's after
// the first claim's broadcast, in either order: both land, with the nonces 5
// and 6, and each claim reports as it would alone. A broadcast takes a moment
// to reach the pool, so a turn given back before the node has answered the
// broadcast lets the next claim read the same nonce.
func TestSnClaimAndSnLedgerClaimTakeTurnsAtTheNonce(t *testing.T) {
	for _, order := range testClaimOrders {
		t.Run(order.name, func(t *testing.T) {
			f := newTestGasKeyClaims(t, newTestV2Claim(t))
			f.pool.pairNonceReads(time.Second)
			f.pool.delayBroadcasts(200 * time.Millisecond)
			v1, v2 := f.claimBoth(t, order.v1First)
			landed := f.requireLanded(t, 2)
			v1Tx, v2Tx := landedTo(t, landed, f.v1.vault), landedTo(t, landed, f.v2.chain.vault2)
			if first := landed[0]; (first == v1Tx) != order.v1First {
				t.Fatalf("nonce 5 went to %x, not to the claim that took the turn first", first.tx.to)
			}
			wantV1 := []testLedgerClaimEvent{
				{kind: "sent", index: 7, txHash: v1Tx.hash},
				{kind: "confirmed", index: 7, txHash: v1Tx.hash, amount: testV1Epoch7Rao},
				{kind: "done"},
			}
			wantV2 := []testLedgerClaimEvent{
				{kind: "sent", noId: 1, index: 2, txHash: v2Tx.hash},
				{kind: "confirmed", noId: 1, index: 2, txHash: v2Tx.hash, amount: testV2Cumulative - testV2Paid},
				{kind: "done"},
			}
			if !reflect.DeepEqual(v1, wantV1) || !reflect.DeepEqual(v2, wantV2) {
				t.Fatalf("v1 claim %+v, want %+v; v2 claim %+v, want %+v", v1, wantV1, v2, wantV2)
			}
			if paid := f.v2.chain.paid(1, 2); paid != testV2Cumulative {
				t.Fatalf("vault2 paid %d", paid)
			}
		})
	}
}

// Two v2 claims started together (a second window's Claim): with the turn the
// second sends with the next nonce, its claim is mined after the first's, and
// vault2's NothingToClaim says the first paid the account (already_claimed,
// success for it), where without the turn the node refused it.
func TestTwoSnLedgerClaimsTakeTurnsAtTheNonce(t *testing.T) {
	f := newTestGasKeyClaims(t, newTestV2Claim(t))
	f.pool.pairNonceReads(time.Second)
	first, second := newRecordingLedgerClaimCallback(), newRecordingLedgerClaimCallback()
	f.pool.startAtFirstNonceRead(func() { f.device.SnLedgerClaim(second) })
	f.device.SnLedgerClaim(first)
	claims := [][]testLedgerClaimEvent{first.wait(t), second.wait(t)}
	landed := f.requireLanded(t, 2)
	// the claim whose transaction took nonce 5 paid the account
	if 0 < len(claims[1]) && claims[1][0].txHash == landed[0].hash {
		claims[0], claims[1] = claims[1], claims[0]
	}
	wantPaid := []testLedgerClaimEvent{
		{kind: "sent", noId: 1, index: 2, txHash: landed[0].hash},
		{kind: "confirmed", noId: 1, index: 2, txHash: landed[0].hash, amount: testV2Cumulative - testV2Paid},
		{kind: "done"},
	}
	wantAfter := []testLedgerClaimEvent{
		{kind: "sent", noId: 1, index: 2, txHash: landed[1].hash},
		{kind: "failed", noId: 1, index: 2, message: SnErrorCodeAlreadyClaimed + ": NothingToClaim: another claim paid this account first"},
		{kind: "done"},
	}
	if !reflect.DeepEqual(claims[0], wantPaid) || !reflect.DeepEqual(claims[1], wantAfter) {
		t.Fatalf("claims %+v, want %+v and %+v", claims, wantPaid, wantAfter)
	}
	if paid := f.v2.chain.paid(1, 2); paid != testV2Cumulative {
		t.Fatalf("vault2 paid %d", paid)
	}
}

// A v2 claim of two pools in one batch beside a v1 claim, in either order: the
// batch takes the gas key's turn too, and both land with the nonces 5 and 6.
func TestSnLedgerClaimBatchAndSnClaimTakeTurnsAtTheNonce(t *testing.T) {
	for _, order := range testClaimOrders {
		t.Run(order.name, func(t *testing.T) {
			v2Fixture, _ := newTestV2BatchClaim(t)
			f := newTestGasKeyClaims(t, v2Fixture)
			f.pool.pairNonceReads(time.Second)
			f.pool.delayBroadcasts(200 * time.Millisecond)
			v1, v2 := f.claimBoth(t, order.v1First)
			landed := f.requireLanded(t, 2)
			v1Tx, batch := landedTo(t, landed, f.v1.vault), landedTo(t, landed, testBatcherAddress)
			if first := landed[0]; (first == v1Tx) != order.v1First {
				t.Fatalf("nonce 5 went to %x, not to the claim that took the turn first", first.tx.to)
			}
			wantV1 := []testLedgerClaimEvent{
				{kind: "sent", index: 7, txHash: v1Tx.hash},
				{kind: "confirmed", index: 7, txHash: v1Tx.hash, amount: testV1Epoch7Rao},
				{kind: "done"},
			}
			wantV2 := []testLedgerClaimEvent{
				{kind: "sent", noId: 1, index: 2, txHash: batch.hash},
				{kind: "sent", noId: testCtfNoId, index: 1, txHash: batch.hash},
				{kind: "confirmed", noId: 1, index: 2, txHash: batch.hash, amount: testV2Cumulative - testV2Paid},
				{kind: "confirmed", noId: testCtfNoId, index: 1, txHash: batch.hash, amount: testCtfCumulative},
				{kind: "done"},
			}
			if !reflect.DeepEqual(v1, wantV1) || !reflect.DeepEqual(v2, wantV2) {
				t.Fatalf("v1 claim %+v, want %+v; v2 claim %+v, want %+v", v1, wantV1, v2, wantV2)
			}
		})
	}
}

// A v2 claim the gas key can pay for only in part, beside a v1 claim: 0.039 TAO
// pays for the batch of the two largest of alice's three accounts but not of all
// three, so the batch sent is the smaller one, signed again with the nonce read
// again (smallerBatch) within the same turn, and pool 1 alone then needs gas. In
// either order the batch and the v1 claim land with the nonces 5 and 6.
func TestSnLedgerClaimSmallerBatchAndSnClaimTakeTurnsAtTheNonce(t *testing.T) {
	balance := int64(39_000_000_000_000_000)
	for _, order := range testClaimOrders {
		t.Run(order.name, func(t *testing.T) {
			f := newTestGasKeyClaims(t, newTestV2ThreeGasKeyClaim(t, balance))
			f.pool.pairNonceReads(time.Second)
			f.pool.delayBroadcasts(200 * time.Millisecond)
			v1, v2 := f.claimBoth(t, order.v1First)
			landed := f.requireLanded(t, 2)
			v1Tx, batch := landedTo(t, landed, f.v1.vault), landedTo(t, landed, testBatcherAddress)
			if first := landed[0]; (first == v1Tx) != order.v1First {
				t.Fatalf("nonce 5 went to %x, not to the claim that took the turn first", first.tx.to)
			}
			if _, claims, _, err := decodeTestClaimMany(batch.tx.data); err != nil || len(claims) != 2 {
				t.Fatalf("the batch sent claims %d accounts, %v", len(claims), err)
			}
			key, err := f.device.snGasPrivateKey()
			if err != nil {
				t.Fatal(err)
			}
			// the batch used 450,000 gas, and what is left pays for no claim alone
			left := balance - (60_000+2*195_000)*testWeiPerGas
			wantV1 := []testLedgerClaimEvent{
				{kind: "sent", index: 7, txHash: v1Tx.hash},
				{kind: "confirmed", index: 7, txHash: v1Tx.hash, amount: testV1Epoch7Rao},
				{kind: "done"},
			}
			wantV2 := []testLedgerClaimEvent{
				{kind: "sent", noId: 2, index: 0, txHash: batch.hash},
				{kind: "sent", noId: testCtfNoId, index: 1, txHash: batch.hash},
				{kind: "confirmed", noId: 2, index: 0, txHash: batch.hash, amount: testThreeAmounts[2]},
				{kind: "confirmed", noId: testCtfNoId, index: 1, txHash: batch.hash, amount: testThreeAmounts[testCtfNoId]},
				{kind: "failed", noId: 1, index: 2, message: snNeedsGasMessage(big.NewInt(testClaimAloneGas*testWeiPerGas), big.NewInt(left), snGasKeyInfo(key))},
				{kind: "done"},
			}
			if !reflect.DeepEqual(v1, wantV1) || !reflect.DeepEqual(v2, wantV2) {
				t.Fatalf("v1 claim %+v, want %+v; v2 claim %+v, want %+v", v1, wantV1, v2, wantV2)
			}
		})
	}
}

// The turn ends when the node has answered the broadcast, not at the receipt:
// here no receipt comes until both claims' transactions reached the pool, so a
// turn held through the receipt wait would keep the second claim from sending
// until the first gave up on its receipt, minutes later.
func TestTheGasKeysTurnEndsAtTheBroadcast(t *testing.T) {
	f := newTestGasKeyClaims(t, newTestV2Claim(t))
	f.pool.holdReceipts(2)
	v1, v2 := f.claimBoth(t, true)
	landed := f.requireLanded(t, 2)
	if len(v1) != 3 || v1[1].kind != "confirmed" || len(v2) != 3 || v2[1].kind != "confirmed" || v1[0].txHash == v2[0].txHash {
		t.Fatalf("v1 claim %+v, v2 claim %+v, landed %d", v1, v2, len(landed))
	}
}

// Nonce reads from another node than the broadcasts', one that counts a held
// transaction only 500 ms after the pool took it, as behind a load balancer or
// after a failover to the next rpc url: the second claim's read does not count
// the first claim's transaction yet. The first's nonce in flight makes the
// second's the next one, in either order. No receipt comes until both are
// sent, so the first is not mined before the second reads.
func TestClaimsTakeTurnsWhenNonceReadsLagTheBroadcasts(t *testing.T) {
	for _, c := range []struct {
		name  string
		order bool
		v2    func(t *testing.T) *testV2Claim
	}{
		{"the v1 claim first", true, newTestV2Claim},
		{"the v2 claim first", false, newTestV2Claim},
		{"a v2 batch first", false, func(t *testing.T) *testV2Claim { f, _ := newTestV2BatchClaim(t); return f }},
	} {
		order := struct{ v1First bool }{c.order}
		t.Run(c.name, func(t *testing.T) {
			f := newTestGasKeyClaims(t, c.v2(t))
			f.pool.lagReads(500 * time.Millisecond)
			f.pool.holdReceipts(2)
			v1, v2 := f.claimBoth(t, order.v1First)
			landed := f.requireLanded(t, 2)
			v1Tx := landedTo(t, landed, f.v1.vault)
			if (landed[0] == v1Tx) != order.v1First {
				t.Fatalf("nonce 5 went to %x, not to the claim that took the turn first", landed[0].tx.to)
			}
			if len(v1) != 3 || v1[1].kind != "confirmed" || v1[1].txHash != v1Tx.hash || len(v2) < 3 || v2[len(v2)-2].kind != "confirmed" {
				t.Fatalf("v1 claim %+v, v2 claim %+v", v1, v2)
			}
			f.requireTurnFree(t, "both claims")
		})
	}
}

// A broadcast the node refused holds no nonce: the first claim's transaction
// is refused and nothing of it is in flight, so the second claim, which waited
// for the turn, sends with the same nonce and lands.
func TestARefusedBroadcastHoldsNoNonce(t *testing.T) {
	f := newTestGasKeyClaims(t, newTestV2Claim(t))
	f.pool.refuse(1, "transaction underpriced")
	v1, v2 := f.claimBoth(t, true)
	landed, refused := f.pool.results(f.gasKey)
	if len(refused) != 1 || len(landed) != 1 || landed[0].tx.nonce != 5 || landed[0].tx.to != f.v2.chain.vault2 {
		t.Fatalf("landed %d (the first to %x with nonce %d), refused %q", len(landed), landed[0].tx.to, landed[0].tx.nonce, refused)
	}
	wantV1 := []testLedgerClaimEvent{
		{kind: "failed", index: 7, message: SnErrorCodeClaimFailed + ": transaction underpriced"},
		{kind: "done"},
	}
	wantV2 := []testLedgerClaimEvent{
		{kind: "sent", noId: 1, index: 2, txHash: landed[0].hash},
		{kind: "confirmed", noId: 1, index: 2, txHash: landed[0].hash, amount: testV2Cumulative - testV2Paid},
		{kind: "done"},
	}
	if !reflect.DeepEqual(v1, wantV1) || !reflect.DeepEqual(v2, wantV2) {
		t.Fatalf("v1 claim %+v, want %+v; v2 claim %+v, want %+v", v1, wantV1, v2, wantV2)
	}
	f.requireTurnFree(t, "both claims")
}

// Two v1 claims started together (a second window's Claim) take turns too and
// land with the nonces 5 and 6. The v1 fake pays every claim it mines, where the
// vault would revert the second as AlreadyClaimed, for its nonce all the same.
func TestTwoSnClaimsTakeTurnsAtTheNonce(t *testing.T) {
	f := newTestGasKeyClaims(t, newTestV2Claim(t))
	f.pool.pairNonceReads(time.Second)
	f.pool.delayBroadcasts(200 * time.Millisecond)
	epochs := NewInt64List()
	epochs.Add(7)
	first, second := newRecordingEpochClaimCallback(), newRecordingEpochClaimCallback()
	f.pool.startAtFirstNonceRead(func() { f.device.SnClaim(epochs, second) })
	f.device.SnClaim(epochs, first)
	claims := [][]testLedgerClaimEvent{first.wait(t), second.wait(t)}
	landed := f.requireLanded(t, 2)
	for i, claim := range claims {
		want := []testLedgerClaimEvent{
			{kind: "sent", index: 7, txHash: landed[i].hash},
			{kind: "confirmed", index: 7, txHash: landed[i].hash, amount: testV1Epoch7Rao},
			{kind: "done"},
		}
		if !reflect.DeepEqual(claim, want) {
			t.Fatalf("claim %d %+v, want %+v (the first claim takes the turn first)", i, claim, want)
		}
	}
}

// Two devices of one process on one network space share its gas key, as a
// DeviceLocal and a DeviceRemote do: a v1 claim on one and a v2 claim on the
// other, started together, take turns as the claims of one device do.
func TestTwoDevicesSharingAGasKeyTakeTurns(t *testing.T) {
	f := newTestGasKeyClaims(t, newTestV2Claim(t))
	other := f.anotherDevice()
	if other.GetSnGasKey().Address != f.device.GetSnGasKey().Address || other.state == f.device.state {
		t.Fatal("not another device with the same gas key")
	}
	f.pool.pairNonceReads(time.Second)
	f.pool.delayBroadcasts(200 * time.Millisecond)
	epochs := NewInt64List()
	epochs.Add(7)
	v1, v2 := newRecordingEpochClaimCallback(), newRecordingLedgerClaimCallback()
	f.pool.startAtFirstNonceRead(func() { other.SnLedgerClaim(v2) })
	f.device.SnClaim(epochs, v1)
	v1Events, v2Events := v1.wait(t), v2.wait(t)
	landed := f.requireLanded(t, 2)
	if landed[0].tx.to != f.v1.vault || len(v1Events) != 3 || v1Events[1].kind != "confirmed" || len(v2Events) != 3 || v2Events[1].kind != "confirmed" {
		t.Fatalf("v1 claim %+v, v2 claim %+v", v1Events, v2Events)
	}
}

// Every way a claim ends gives the gas key's turn back and leaves nothing of it
// in flight: confirmed, short of gas, its nonce unreadable, its broadcast
// refused, and a batch the gas key cannot pay for, whose accounts are then
// claimed alone.
func TestEveryClaimGivesTheGasKeysTurnBack(t *testing.T) {
	epochs := NewInt64List()
	epochs.Add(7)
	claimV1 := func(t *testing.T, f *testGasKeyClaims) []testLedgerClaimEvent {
		callback := newRecordingEpochClaimCallback()
		f.device.SnClaim(epochs, callback)
		return callback.wait(t)
	}
	claimV2 := func(t *testing.T, f *testGasKeyClaims) []testLedgerClaimEvent {
		callback := newRecordingLedgerClaimCallback()
		f.device.SnLedgerClaim(callback)
		return callback.wait(t)
	}
	setBalance := func(f *testGasKeyClaims, wei int64) {
		f.v2.chain.mutex.Lock()
		defer f.v2.chain.mutex.Unlock()
		f.v2.chain.balance = big.NewInt(wei)
	}
	for _, c := range []struct {
		name string
		v2   func(t *testing.T) *testV2Claim
		set  func(f *testGasKeyClaims)
		// the claims and what each must have reported
		claims []func(t *testing.T, f *testGasKeyClaims) []testLedgerClaimEvent
		want   []string
	}{
		{"confirmed", newTestV2Claim, func(*testGasKeyClaims) {},
			[]func(*testing.T, *testGasKeyClaims) []testLedgerClaimEvent{claimV1, claimV2}, []string{"confirmed", "confirmed"}},
		{"short of gas", newTestV2Claim, func(f *testGasKeyClaims) { setBalance(f, 10_000_000_000_000_000) },
			[]func(*testing.T, *testGasKeyClaims) []testLedgerClaimEvent{claimV1, claimV2}, []string{SnErrorCodeNeedsGas + ": ", SnErrorCodeNeedsGas + ": "}},
		{"the nonce unreadable", newTestV2Claim, func(f *testGasKeyClaims) { f.pool.failNonceReads(true) },
			[]func(*testing.T, *testGasKeyClaims) []testLedgerClaimEvent{claimV1, claimV2}, []string{SnErrorCodeClaimFailed + ": nonce unavailable", SnErrorCodeClaimFailed + ": nonce unavailable"}},
		{"the broadcast refused", newTestV2Claim, func(f *testGasKeyClaims) { f.pool.refuse(2, "transaction underpriced") },
			[]func(*testing.T, *testGasKeyClaims) []testLedgerClaimEvent{claimV1, claimV2}, []string{SnErrorCodeClaimFailed + ": transaction underpriced", SnErrorCodeClaimFailed + ": transaction underpriced"}},
		// 0.036 TAO pays for one claim alone (0.0346 TAO) but not for the batch
		{"a batch the gas key cannot pay for", func(t *testing.T) *testV2Claim { f, _ := newTestV2BatchClaim(t); return f }, func(f *testGasKeyClaims) { setBalance(f, 36_000_000_000_000_000) },
			[]func(*testing.T, *testGasKeyClaims) []testLedgerClaimEvent{claimV2}, []string{"confirmed"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newTestGasKeyClaims(t, c.v2(t))
			c.set(f)
			for i, claim := range c.claims {
				events := claim(t, f)
				outcome := events[len(events)-2]
				if outcome.kind != c.want[i] && !(outcome.kind == "failed" && strings.HasPrefix(outcome.message, c.want[i])) {
					t.Fatalf("claim %d: %+v, want %q", i, events, c.want[i])
				}
				f.requireTurnFree(t, fmt.Sprintf("claim %d", i))
			}
			// the batch was not sent: each account went alone
			landed, _ := f.pool.results(f.gasKey)
			for _, held := range landed {
				if held.tx.to == testBatcherAddress {
					t.Fatalf("a batch was sent with nonce %d", held.tx.nonce)
				}
			}
		})
	}
}

// A transaction a node accepted holds its nonce while its claim waits for the
// receipt, with the turn free for another claim, and not after: a claim whose
// wait ends early leaves nothing in flight, so once the pool drops its
// transaction, which never mines, the next claim sends with the same nonce.
func TestATransactionHoldsItsNonceOnlyWhileItsClaimWaitsForItsReceipt(t *testing.T) {
	f := newTestGasKeyClaims(t, newTestV2Claim(t))
	f.pool.holdReceipts(1_000)
	key, err := f.device.snGasPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	engine, snErr := f.device.snLedgerClaimEngine()
	if snErr != nil {
		t.Fatal(snErr.Message)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	callback := newRecordingLedgerClaimCallback()
	done := make(chan struct{})
	go func() {
		defer close(done)
		engine.claim(ctx, key, callback)
	}()
	deadline := time.Now().Add(30 * time.Second)
	for events := callback.recorded(); len(events) == 0 || events[0].kind != "sent"; events = callback.recorded() {
		if 0 < len(events) || time.Now().After(deadline) {
			t.Fatalf("the claim did not send: %+v", events)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// it waits for its receipt: nonce 5 is in flight and the turn is free
	if floor := snGasKeyTurns.floor(f.gasKey); floor != 6 {
		t.Fatalf("in flight below %d, want nonce 5", floor)
	}
	turnCtx, turnCancel := context.WithTimeout(context.Background(), 5*time.Second)
	turn, err := snGasKeyTurns.take(turnCtx, f.gasKey)
	turnCancel()
	if err != nil {
		t.Fatalf("the turn was held through the receipt wait: %v", err)
	}
	turn.release()
	// its wait ends early: nothing is in flight any more
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the claim went on after its wait was cancelled")
	}
	if events := callback.recorded(); len(events) != 2 || events[1].kind != "failed" {
		t.Fatalf("events %+v", events)
	}
	f.requireTurnFree(t, "the claim's wait ended")
	// the pool drops its transaction, which never mines: the next claim takes
	// nonce 5
	if dropped := f.pool.drop(f.gasKey); dropped != 1 {
		t.Fatalf("dropped %d", dropped)
	}
	f.pool.holdReceipts(0)
	next := newRecordingLedgerClaimCallback()
	f.device.SnLedgerClaim(next)
	if events := next.wait(t); len(events) != 3 || events[1].kind != "confirmed" {
		t.Fatalf("the next claim %+v", events)
	}
	f.requireLanded(t, 1)
}

// testTurnWaitBudget is the time of a claim that waits for a turn another
// holds: far more than its reads before the turn take, so its time runs out
// while it waits.
const testTurnWaitBudget = 3 * time.Second

// runOutOfTime runs claim with a context that ends after testTurnWaitBudget,
// and fails when the claim has not returned long after: a wait for the turn
// that its context does not end.
func runOutOfTime(t *testing.T, claim func(ctx context.Context)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTurnWaitBudget)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		claim(ctx)
	}()
	select {
	case <-done:
	case <-time.After(10 * testTurnWaitBudget):
		t.Fatal("a claim waiting for the gas key's turn went on after its time ran out")
	}
}

// A claim whose time runs out while another holds the gas key's turn sends
// nothing and fails with why; once the turn is given back, a claim sends.
func TestAClaimThatCannotGetTheGasKeysTurnSendsNothing(t *testing.T) {
	f := newTestGasKeyClaims(t, newTestV2Claim(t))
	key, err := f.device.snGasPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	held, err := snGasKeyTurns.take(context.Background(), f.gasKey)
	if err != nil {
		t.Fatal(err)
	}
	wantFailure := SnErrorCodeClaimFailed + ": waiting for another claim from the gas key to send: " + context.DeadlineExceeded.Error()

	v2Engine, snErr := f.device.snLedgerClaimEngine()
	if snErr != nil {
		t.Fatal(snErr.Message)
	}
	v2 := newRecordingLedgerClaimCallback()
	runOutOfTime(t, func(ctx context.Context) { v2Engine.claim(ctx, key, v2) })
	if events := v2.recorded(); len(events) != 1 || events[0].kind != "failed" || events[0].noId != 1 || events[0].index != 2 || events[0].message != wantFailure {
		t.Fatalf("v2 claim %+v, want one failure %q", events, wantFailure)
	}

	v1Engine, snErr := f.device.snClaimEngine()
	if snErr != nil {
		t.Fatal(snErr.Message)
	}
	v1 := newRecordingEpochClaimCallback()
	runOutOfTime(t, func(ctx context.Context) { v1Engine.send(ctx, key, 7, v1) })
	if events := v1.recorded(); len(events) != 1 || events[0].kind != "failed" || events[0].index != 7 || events[0].message != wantFailure {
		t.Fatalf("v1 claim %+v, want one failure %q", events, wantFailure)
	}
	if sent := f.pool.sentCount(); sent != 0 {
		t.Fatalf("%d sent while another held the turn", sent)
	}

	held.release()
	v2 = newRecordingLedgerClaimCallback()
	f.device.SnLedgerClaim(v2)
	if events := v2.wait(t); len(events) != 3 || events[1].kind != "confirmed" {
		t.Fatalf("after the turn was given back: %+v", events)
	}
	f.requireLanded(t, 1)
}

// A batch whose time runs out while another holds the gas key's turn sends
// nothing either: its accounts go to be claimed alone, and those find the claim's
// time gone too.
func TestABatchThatCannotGetTheGasKeysTurnSendsNothing(t *testing.T) {
	v2Fixture, _ := newTestV2BatchClaim(t)
	f := newTestGasKeyClaims(t, v2Fixture)
	key, err := f.device.snGasPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	held, err := snGasKeyTurns.take(context.Background(), f.gasKey)
	if err != nil {
		t.Fatal(err)
	}
	defer held.release()
	engine, snErr := f.device.snLedgerClaimEngine()
	if snErr != nil {
		t.Fatal(snErr.Message)
	}
	callback := newRecordingLedgerClaimCallback()
	runOutOfTime(t, func(ctx context.Context) { engine.claim(ctx, key, callback) })
	events := callback.recorded()
	if len(events) != 2 || events[0].kind != "failed" || events[1].kind != "failed" || events[0].noId == events[1].noId {
		t.Fatalf("events %+v, want each account failed", events)
	}
	if sent := f.pool.sentCount(); sent != 0 {
		t.Fatalf("%d sent while another held the turn", sent)
	}
}

// The turns themselves: one at a time for an address, none between
// addresses, a wait that ctx ends takes nothing, and a release called twice
// gives back only its own turn.
func TestSnKeyTurns(t *testing.T) {
	turns := newSnKeyTurns()
	a, b := [20]byte{0xa}, [20]byte{0xb}
	turnA, err := turns.take(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if turnB, err := turns.take(context.Background(), b); err != nil {
		t.Fatalf("another address waited: %v", err)
	} else {
		turnB.release()
	}

	// a wait that ctx ends takes nothing
	waitEnds := func(what string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		ended := make(chan error, 1)
		go func() {
			_, err := turns.take(ctx, a)
			ended <- err
		}()
		select {
		case err := <-ended:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s: %v", what, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the wait went on after its context ended", what)
		}
	}
	waitEnds("a taken turn was taken again")
	// a context already ended takes no turn, not even a free one (a select
	// with both ready would take it half the time)
	ended, end := context.WithCancel(context.Background())
	end()
	for i := 0; i < 100; i++ {
		if turn, err := turns.take(ended, b); err == nil {
			turn.release()
			t.Fatal("a context already ended took a free turn")
		} else if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}

	// the next waits for the release
	taken := make(chan *snGasKeyTurn, 1)
	go func() {
		turn, err := turns.take(context.Background(), a)
		if err != nil {
			t.Error(err)
		}
		taken <- turn
	}()
	select {
	case <-taken:
		t.Fatal("a taken turn was taken again")
	case <-time.After(100 * time.Millisecond):
	}
	turnA.release()
	var turnNext *snGasKeyTurn
	select {
	case turnNext = <-taken:
	case <-time.After(5 * time.Second):
		t.Fatal("the released turn was not taken")
	}

	// a second release of the first turn gives back nothing of the next's
	turnA.release()
	waitEnds("a release called twice gave back the next turn")
	turnNext.release()
	if turn, err := turns.take(context.Background(), a); err != nil {
		t.Fatal(err)
	} else {
		turn.release()
	}
}
