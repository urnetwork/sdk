package sdk

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"

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
// (snGasKeyTurn): one at a time for a key, from the nonce read until a node
// has answered the broadcast, and never through the receipt wait, so a claim
// waiting for the turn only starts its send a little later. A turn lasts for
// the signing's chain reads and the broadcast, and for a batch the gas key
// cannot pay for whole, the sizing of a smaller one too (smallerBatch).
//
// A transaction a node accepted stays in flight until its claim's receipt
// wait ends (snGasKeyTurn.sent), and a turn's nonce is the node's pending count
// or one past the highest nonce in flight from the key, whichever is higher
// (snGasKeyTurn.nonce). So:
//   - two claims whose transactions are in flight together never share a
//     nonce, whatever node answers the reads: the later turn starts after the
//     earlier one recorded its transaction, which a node other than the one
//     that took the broadcast may not count yet (behind a load balancer,
//     after a failover to the next rpc url), nor a node just starting;
//   - a transaction a node refused holds no nonce and is not recorded, so the
//     next claim reads the pending count alone and takes the nonce again;
//   - nothing is kept past a receipt wait: a transaction the pool dropped holds
//     back the nonces after it only until its claim stops waiting for it, and
//     a transaction mined, or one sent from the key in another process, is the
//     node's to count.
// The turns are the process's, by the gas key's address, so two devices or
// windows in one process that share a gas key take turns too.

type snKeyTurns struct {
	mutex sync.Mutex
	// one turn for each address that has sent, kept for the process (a device
	// has one gas key): it holds a token while the turn is taken
	turns map[[20]byte]chan struct{}
	// the nonces of the transactions from each address that a node accepted
	// and whose claims still wait for their receipts
	inFlight map[[20]byte]map[uint64]bool
}

var snGasKeyTurns = newSnKeyTurns()

func newSnKeyTurns() *snKeyTurns {
	return &snKeyTurns{
		turns:    map[[20]byte]chan struct{}{},
		inFlight: map[[20]byte]map[uint64]bool{},
	}
}

// snGasKeyTurn is an address's turn, taken (snKeyTurns.take). Its
// transaction is signed with nonce and recorded with sent once a node accepted
// it, and release gives the turn back.
type snGasKeyTurn struct {
	turns   *snKeyTurns
	address [20]byte
	token   chan struct{}
	once    sync.Once
	// the turn's nonce, read once
	nonceRead bool
	nonceNext uint64
}

// take waits for address's turn and takes it. ctx ends the wait with its
// error, and then no turn is taken, even a free one.
func (self *snKeyTurns) take(ctx context.Context, address [20]byte) (*snGasKeyTurn, error) {
	waitErr := func() error {
		return fmt.Errorf("waiting for another claim from the gas key to send: %w", ctx.Err())
	}
	if ctx.Err() != nil {
		return nil, waitErr()
	}
	self.mutex.Lock()
	token := self.turns[address]
	if token == nil {
		token = make(chan struct{}, 1)
		self.turns[address] = token
	}
	self.mutex.Unlock()
	select {
	case token <- struct{}{}:
	case <-ctx.Done():
		return nil, waitErr()
	}
	return &snGasKeyTurn{turns: self, address: address, token: token}, nil
}

// floor is one past the highest nonce in flight from address, 0 with none.
func (self *snKeyTurns) floor(address [20]byte) uint64 {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	floor := uint64(0)
	for nonce := range self.inFlight[address] {
		floor = max(floor, nonce+1)
	}
	return floor
}

// snTakeGasKeyTurn waits for the gas key's turn (snKeyTurns.take).
func snTakeGasKeyTurn(ctx context.Context, key *secp.PrivateKey) (*snGasKeyTurn, error) {
	return snGasKeyTurns.take(ctx, evm.AddressOf(key))
}

// nonce is the nonce of the turn's transaction: the node's pending count, or
// one past the highest nonce in flight from the key, whichever is higher. It is
// read once a turn; the turn's every signature uses it.
func (self *snGasKeyTurn) nonce(ctx context.Context, client *evm.Client) (uint64, error) {
	if self.nonceRead {
		return self.nonceNext, nil
	}
	pending, err := client.Nonce(ctx, self.address)
	if err != nil {
		return 0, err
	}
	self.nonceRead, self.nonceNext = true, max(pending, self.turns.floor(self.address))
	return self.nonceNext, nil
}

// sent records that a node accepted the turn's transaction, before the turn
// is given back. The settle it returns ends the record, at the end of the
// claim's receipt wait whatever came of it; calling it again does nothing.
func (self *snGasKeyTurn) sent() (settle func()) {
	if !self.nonceRead {
		return func() {}
	}
	nonce := self.nonceNext
	self.turns.mutex.Lock()
	if self.turns.inFlight[self.address] == nil {
		self.turns.inFlight[self.address] = map[uint64]bool{}
	}
	self.turns.inFlight[self.address][nonce] = true
	self.turns.mutex.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			self.turns.mutex.Lock()
			defer self.turns.mutex.Unlock()
			delete(self.turns.inFlight[self.address], nonce)
			if len(self.turns.inFlight[self.address]) == 0 {
				delete(self.turns.inFlight, self.address)
			}
		})
	}
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
