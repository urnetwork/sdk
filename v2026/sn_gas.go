package sdk

import (
	"context"
	"errors"
	"math/big"
	"strings"

	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026/sn/evm"
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
