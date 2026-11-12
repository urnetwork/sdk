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

// The gas key's turns. Each transaction the gas key sends takes its nonce
// from the node's pending count, eth_getTransactionCount(pending), as it is
// signed (snSignClaim). A v1 claim (SnClaim) and a v2 claim (SnLedgerClaim)
// each send from their own goroutine, and so does a second claim of either
// kind started meanwhile, so two of them could read the same pending nonce
// before either was broadcast: one transaction then replaces the other, or
// the node refuses it. So every send holds its gas key's turn from the nonce
// read until the node has answered its broadcast (snSendClaim, and
// snLedgerClaimEngine.broadcastBatch), and gives it up before the receipt
// wait. The next send's nonce read then comes after the earlier transaction
// reached the node's pool, whose pending count includes it, so it reads the
// next nonce; a transaction the node refused holds no nonce, and the next
// read is right without it. A claim waiting for the turn only starts its send
// a little later: a turn lasts for the signing's few chain reads and the
// broadcast (for a batch the gas key cannot pay for whole, the sizing of a
// smaller one too), never for a block. Nothing is kept from one turn to the
// next, so no nonce held here can go stale: a transaction the pool drops, or
// one sent from the key in another process, is the node's to count. The turns
// are the process's, by the gas key's address, so two devices or windows in
// one process that share a gas key take turns too.

type snKeyTurns struct {
	mutex sync.Mutex
	// one turn for each address that has sent, kept for the process (a device
	// has one gas key): it holds a token while the turn is taken
	turns map[[20]byte]chan struct{}
}

var snGasKeyTurns = &snKeyTurns{turns: map[[20]byte]chan struct{}{}}

// take waits for address's turn and takes it. The release it returns gives
// the turn back; calling it again does nothing. ctx ends the wait with its
// error, and then no turn is taken.
func (self *snKeyTurns) take(ctx context.Context, address [20]byte) (func(), error) {
	self.mutex.Lock()
	turn := self.turns[address]
	if turn == nil {
		turn = make(chan struct{}, 1)
		self.turns[address] = turn
	}
	self.mutex.Unlock()
	select {
	case turn <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for another claim from the gas key to send: %w", ctx.Err())
	}
	var release sync.Once
	return func() {
		release.Do(func() { <-turn })
	}, nil
}

// snGasKeyTurn waits for the gas key's turn to sign and send a transaction
// (snKeyTurns.take).
func snGasKeyTurn(ctx context.Context, key *secp.PrivateKey) (func(), error) {
	return snGasKeyTurns.take(ctx, evm.AddressOf(key))
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
