package sdk

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk/sn/evm"
)

// The gas key is an SDK-generated EVM key that only ever pays gas for
// claim() transactions. Its secret lives in local state and is never
// exported; the app sees the address and the ss58 mirror to fund it with
// TAO. Claims pay the coldkey regardless of who sends them, so the gas key
// holds no alpha.
type SnGasKey struct {
	Address    string `json:"address"`
	MirrorSs58 string `json:"mirror_ss58"`
}

func snGasKeyInfo(key *secp.PrivateKey) *SnGasKey {
	address := evm.AddressHex(evm.AddressOf(key))
	return &SnGasKey{
		Address:    address,
		MirrorSs58: EvmMirrorSs58(address),
	}
}

func (self *snDevice) snGasPrivateKey() (*secp.PrivateKey, error) {
	self.state.mutex.Lock()
	defer self.state.mutex.Unlock()
	if self.state.gasKey != nil {
		return self.state.gasKey, nil
	}
	localState := self.snLocalState()
	if localState == nil {
		return nil, errors.New(SnErrorCodeLocalState)
	}
	if secret := localState.getSnGasKeySecret(); secret != nil {
		if key, err := evm.PrivateKeyFromBytes(secret); err == nil {
			self.state.gasKey = key
			return key, nil
		}
	}
	key, err := evm.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	if err := localState.setSnGasKeySecret(key.Serialize()); err != nil {
		return nil, err
	}
	self.state.gasKey = key
	return key, nil
}

// GetSnGasKey returns the device gas key, creating it on first use. Nil when
// local state is unavailable.
func (self *snDevice) GetSnGasKey() *SnGasKey {
	key, err := self.snGasPrivateKey()
	if err != nil {
		return nil
	}
	return snGasKeyInfo(key)
}

// The gas key's turns. A v1 claim (SnClaim) and a v2 claim (SnLedgerClaim)
// each send from their own goroutine, and so does a second claim of either
// kind started meanwhile. Each transaction took its nonce from the node's
// pending count, eth_getTransactionCount(pending), as it was signed, so two
// claims could read the same nonce before either was broadcast: one
// transaction then replaced the other, or a node refused it.
//
// So a transaction from the gas key is signed and sent within the key's turn
// (snGasKeyTurn): one at a time for a key on a chain, from the nonce read
// until a node has answered the broadcast, and never through the receipt wait,
// so a claim waiting for the turn only starts its send a little later. A turn
// lasts for the signing's chain reads and the broadcast, and for a batch the
// gas key cannot pay for whole, the sizing of a smaller one too (smallerBatch).
//
// A node's pending count counts what its own pool holds, but a node other than
// the one that took a broadcast (behind a load balancer, after a failover to
// the next rpc url) counts it only once gossip brings it, and a node just
// starting may count it later still. So for snGasKeyInFlightSpan after a node
// accepted a transaction, the key's next nonce is at least one past it
// (snGasKeyTurn.sent), and a turn's nonce is that floor or the pending count,
// whichever is higher (snGasKeyTurn.nonce). So:
//   - two sends from a key less than the span apart, as two claims started
//     together send, never share a nonce, whatever node answers the reads;
//   - after the span the pending count is trusted again: gossip has brought
//     the transaction, or it was mined, or the pool dropped it (as when a
//     block leaves the key unable to pay for it), and then the next send takes
//     its nonce again, so a dropped transaction holds back the sends after it
//     for its span at most;
//   - a transaction whose broadcast failed, refused or lost, raises no floor.
// The turns are the process's, by the gas key's address and chain, so two
// devices or windows in one process that share a gas key take turns too.

// snGasKeyInFlightSpan is how long a transaction a node accepted raises the
// floor of its key's next nonce: several times the gossip between nodes, which
// pass a transaction on as their pool takes it, and less than the 3 s after
// which a claim's own receipt wait first reads the receipt and the claim can
// send again (snClaimReceiptPoll).
const snGasKeyInFlightSpan = 2 * time.Second

// snGasKeyId names a gas key's nonces: its address on one chain.
type snGasKeyId struct {
	chainId int64
	address [20]byte
}

type snKeyTurns struct {
	mutex sync.Mutex
	// one turn for each key that has sent, kept for the process (a device has
	// one gas key): it holds a token while the turn is taken
	turns map[snGasKeyId]chan struct{}
	// the transactions from each key that a node accepted less than span ago
	sent map[snGasKeyId][]snSentNonce
	// snGasKeyInFlightSpan, which tests change
	span time.Duration
}

type snSentNonce struct {
	nonce uint64
	at    time.Time
}

var snGasKeyTurns = newSnKeyTurns()

func newSnKeyTurns() *snKeyTurns {
	return &snKeyTurns{
		turns: map[snGasKeyId]chan struct{}{},
		sent:  map[snGasKeyId][]snSentNonce{},
		span:  snGasKeyInFlightSpan,
	}
}

// snGasKeyTurn is a key's turn, taken (snKeyTurns.take). Its transaction is
// signed with nonce, recorded with sent once a node accepted it, and release
// gives the turn back.
type snGasKeyTurn struct {
	turns *snKeyTurns
	id    snGasKeyId
	token chan struct{}
	once  sync.Once
	// the turn's nonce, read once
	nonceRead bool
	nonceNext uint64
}

// take waits for id's turn and takes it. ctx ends the wait with its error,
// and then no turn is taken, even a free one.
func (self *snKeyTurns) take(ctx context.Context, id snGasKeyId) (*snGasKeyTurn, error) {
	waitErr := func() error {
		return fmt.Errorf("waiting for another claim from the gas key to send: %w", ctx.Err())
	}
	if ctx.Err() != nil {
		return nil, waitErr()
	}
	self.mutex.Lock()
	token := self.turns[id]
	if token == nil {
		token = make(chan struct{}, 1)
		self.turns[id] = token
	}
	self.mutex.Unlock()
	select {
	case token <- struct{}{}:
	case <-ctx.Done():
		return nil, waitErr()
	}
	return &snGasKeyTurn{turns: self, id: id, token: token}, nil
}

// forget drops id's sends the span or more before now. Locked.
func (self *snKeyTurns) forget(id snGasKeyId, now time.Time) {
	var kept []snSentNonce
	for _, sent := range self.sent[id] {
		if now.Sub(sent.at) < self.span {
			kept = append(kept, sent)
		}
	}
	if len(kept) == 0 {
		delete(self.sent, id)
	} else {
		self.sent[id] = kept
	}
}

// floor is one past the highest nonce id sent less than the span before now,
// 0 with none.
func (self *snKeyTurns) floor(id snGasKeyId, now time.Time) uint64 {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.forget(id, now)
	floor := uint64(0)
	for _, sent := range self.sent[id] {
		floor = max(floor, sent.nonce+1)
	}
	return floor
}

// snTakeGasKeyTurn waits for the gas key's turn on the chain chainId
// (snKeyTurns.take).
func snTakeGasKeyTurn(ctx context.Context, chainId *big.Int, key *secp.PrivateKey) (*snGasKeyTurn, error) {
	return snGasKeyTurns.take(ctx, snGasKeyId{chainId: chainId.Int64(), address: evm.AddressOf(key)})
}

// nonce is the nonce of the turn's transaction: the node's pending count, or
// one past the highest nonce the key sent less than the span ago, whichever is
// higher. It is read once a turn; the turn's every signature uses it.
func (self *snGasKeyTurn) nonce(ctx context.Context, client *evm.Client) (uint64, error) {
	if self.nonceRead {
		return self.nonceNext, nil
	}
	// the floor first: a send that ages out of it meanwhile still counts
	floor := self.turns.floor(self.id, time.Now())
	pending, err := client.Nonce(ctx, self.id.address)
	if err != nil {
		return 0, err
	}
	self.nonceRead, self.nonceNext = true, max(pending, floor)
	return self.nonceNext, nil
}

// sent records that a node accepted the turn's transaction, before the turn
// is given back: for the span after, the key's next nonce is at least one past
// it.
func (self *snGasKeyTurn) sent() {
	if !self.nonceRead {
		return
	}
	now := time.Now()
	self.turns.mutex.Lock()
	defer self.turns.mutex.Unlock()
	self.turns.forget(self.id, now)
	self.turns.sent[self.id] = append(self.turns.sent[self.id], snSentNonce{nonce: self.nonceNext, at: now})
}

// release gives the turn back; calling it again does nothing.
func (self *snGasKeyTurn) release() {
	self.once.Do(func() { <-self.token })
}

type SnGasBalanceResult struct {
	Address string   `json:"address"`
	Wei     string   `json:"wei"`
	Tao     float64  `json:"tao"`
	Error   *SnError `json:"error,omitempty"`
}

type SnGasBalanceCallback connect.ApiCallback[*SnGasBalanceResult]

// SnGasBalance reads the gas key's balance on the subtensor EVM.
func (self *snDevice) SnGasBalance(callback SnGasBalanceCallback) {
	go connect.HandleError(func() {
		var result *SnGasBalanceResult
		if gasKey := self.GetSnGasKey(); gasKey == nil {
			result = &SnGasBalanceResult{Error: newSnError(SnErrorCodeLocalState, "gas key unavailable")}
		} else {
			ctx, cancel := context.WithTimeout(self.ctx, snRpcTimeout)
			defer cancel()
			result = snGasBalance(ctx, self.GetSnChainSettings(), gasKey.Address)
		}
		if callback != nil {
			callback.Result(result, nil)
		}
	})
}

// SnGasBalanceFor reads any address's balance with explicit settings (hosts
// without a DeviceLocal, e.g. the web app).
func SnGasBalanceFor(settings *SnChainSettings, address string, callback SnGasBalanceCallback) {
	go connect.HandleError(func() {
		ctx, cancel := context.WithTimeout(context.Background(), snRpcTimeout)
		defer cancel()
		result := snGasBalance(ctx, settings, address)
		if callback != nil {
			callback.Result(result, nil)
		}
	})
}

var snWeiPerTao = new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))

func snGasBalance(ctx context.Context, settings *SnChainSettings, address string) *SnGasBalanceResult {
	result := &SnGasBalanceResult{Address: strings.TrimSpace(address), Wei: "0"}
	if settings == nil || len(settings.rpcUrls) == 0 {
		result.Error = newSnError(SnErrorCodeChainNotConfigured, "no rpc urls")
		return result
	}
	addr, err := evm.ParseAddress(address)
	if err != nil {
		result.Error = newSnError(SnErrorCodeInvalidAddress, err.Error())
		return result
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, snRpcTimeout)
		defer cancel()
	}
	wei, err := settings.chainClient().Balance(ctx, addr)
	if err != nil {
		result.Error = snErrorFromErr(err)
		return result
	}
	result.Wei = wei.String()
	tao, _ := new(big.Float).Quo(new(big.Float).SetInt(wei), snWeiPerTao).Float64()
	result.Tao = tao
	return result
}
