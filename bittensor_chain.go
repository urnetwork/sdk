//go:build !ios_extension && !js

package sdk

// How the sdk builds, signs and sends a Subtensor transaction, for every flow
// a key on the device takes part in. The encoding is internal/subtensor, the
// one Substrate encoder in the sdk. Nothing here is exported: an exported
// function builds one fixed call from typed arguments and comes through here,
// so no caller and no server can choose what gets signed.
//
// What the chain settings can do: name the endpoint. What they cannot: choose
// the chain, the vault or the hotkey. Those are pinned here per chain, and the
// settings and the chain's own answers have to agree with them.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/sdk/internal/subtensor"
	"github.com/urnetwork/sdk/sn/evm"
)

const (
	// half a block: a finalized block is seen soon after it finalizes
	bittensorChainPoll = 6 * time.Second
	// the blocks a transaction can land in, then the finality that shows it did not
	bittensorChainLandTimeout = time.Duration(subtensor.EraPeriodBlocks)*snBlockSeconds*time.Second + 5*time.Minute
)

// What the sdk holds to be true of a chain, whatever an endpoint says.
type bittensorChainPins struct {
	chainId      int64
	genesisHash  [32]byte
	vault2       [20]byte
	escrowHotkey [32]byte
	netuid       uint16
}

var bittensorMainnetPins = bittensorChainPins{
	chainId:      SnChainIdMainnet,
	genesisHash:  bittensorMustHash32("2f0555cc76fc2840a25a6ea3b9637146806f1f44b090c175ffde2a7e5ab36c03"),
	vault2:       [20]byte(bittensorMustHex("e4865eb1bebdf5c6a10c9d37f3ccdd59aee8f20c", 20)),
	escrowHotkey: bittensorMustHash32("a838b2d93ad97cb0fcc91f3bda505ae2bc221b348e47eeb07fd9d0b2ad8dc24b"),
	netuid:       25,
}

func bittensorMustHex(value string, size int) []byte {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != size {
		panic("bad compiled-in value")
	}
	return decoded
}

func bittensorMustHash32(value string) [32]byte {
	return [32]byte(bittensorMustHex(value, 32))
}

// The rpc a chain engine uses. evm.Client is one.
type bittensorChainRpc interface {
	Call(ctx context.Context, method string, params ...any) (json.RawMessage, error)
	Batch(ctx context.Context, calls []evm.BatchCall) ([]evm.BatchResult, error)
}

type bittensorChainEngine struct {
	rpc   bittensorChainRpc
	chain *subtensor.Chain
	pins  bittensorChainPins
	poll  time.Duration
	sleep func(ctx context.Context, wait time.Duration) error
	now   func() time.Time
	// names the endpoints in the read caches; "" reads everything every time
	cacheKey string
}

func bittensorChainSleep(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func newBittensorChainEngineWithRpc(rpc bittensorChainRpc, pins bittensorChainPins) *bittensorChainEngine {
	return &bittensorChainEngine{
		rpc:   rpc,
		chain: subtensor.NewChain(rpc),
		pins:  pins,
		poll:  bittensorChainPoll,
		sleep: bittensorChainSleep,
		now:   time.Now,
	}
}

// newBittensorChainEngine is the engine for the settings. They can name the
// endpoint; a chain, vault or subnet other than the release's refuses.
func newBittensorChainEngine(settings *SnChainSettings) (*bittensorChainEngine, *SnError) {
	if settings == nil || len(settings.rpcUrls) == 0 {
		return nil, newSnError(SnErrorCodeChainNotConfigured, "The Bittensor network is not set up in this app.")
	}
	pins := bittensorMainnetPins
	if settings.ChainId != pins.chainId {
		return nil, newSnError(BittensorChainErrorWrongChain, "This works only on the Bittensor main network.")
	}
	if settings.Netuid != int64(pins.netuid) {
		return nil, newSnError(BittensorChainErrorWrongChain, "These settings are for another subnet.")
	}
	vault2, err := settings.vault2()
	if err != nil || vault2 != pins.vault2 {
		return nil, newSnError(BittensorChainErrorWrongChain, "These settings name another settlement vault.")
	}
	engine := newBittensorChainEngineWithRpc(settings.chainClient(), pins)
	engine.cacheKey = strings.Join(settings.rpcUrls, "\n")
	return engine, nil
}

// One runtime on the pinned chain: its version, its metadata and, when it was
// read for signing, the finalized block it was read at.
type bittensorChainView struct {
	head    subtensor.BlockRef
	version subtensor.RuntimeVersion
	meta    *subtensor.Metadata
}

// A parsed runtime metadata, kept across estimates so a typed amount does not
// fetch about 740 KB again. A transaction never signs under it: signing always
// reads its own. It holds nothing about a key.
var bittensorChainMetadataCache struct {
	sync.Mutex
	cacheKey string
	version  subtensor.RuntimeVersion
	meta     *subtensor.Metadata
}

func (self *bittensorChainEngine) cacheMetadata(version subtensor.RuntimeVersion, meta *subtensor.Metadata) {
	if self.cacheKey == "" {
		return
	}
	bittensorChainMetadataCache.Lock()
	defer bittensorChainMetadataCache.Unlock()
	bittensorChainMetadataCache.cacheKey = self.cacheKey
	bittensorChainMetadataCache.version = version
	bittensorChainMetadataCache.meta = meta
}

func (self *bittensorChainEngine) cachedMetadata(version subtensor.RuntimeVersion) *subtensor.Metadata {
	if self.cacheKey == "" {
		return nil
	}
	bittensorChainMetadataCache.Lock()
	defer bittensorChainMetadataCache.Unlock()
	if bittensorChainMetadataCache.cacheKey != self.cacheKey || bittensorChainMetadataCache.version != version {
		return nil
	}
	return bittensorChainMetadataCache.meta
}

// quoteView is enough to quote: the chain's identity and the vault's hotkey,
// checked against the pins, and the current runtime. It has no block to sign at.
func (self *bittensorChainEngine) quoteView(ctx context.Context) (*bittensorChainView, [32]byte, error) {
	genesis, version, escrowReply, err := self.chain.ReadIdentity(ctx, evm.BatchCall{Method: "eth_call", Params: self.escrowHotkeyParams()})
	if err != nil {
		return nil, [32]byte{}, err
	}
	if genesis != self.pins.genesisHash {
		return nil, [32]byte{}, errBittensorChainWrongChain
	}
	if err := self.checkEscrowHotkey(escrowReply); err != nil {
		return nil, [32]byte{}, err
	}
	meta := self.cachedMetadata(version)
	if meta == nil {
		meta, err = self.chain.CurrentMetadata(ctx, version)
		if err != nil {
			return nil, [32]byte{}, err
		}
		self.cacheMetadata(version, meta)
	}
	return &bittensorChainView{version: version, meta: meta}, self.pins.escrowHotkey, nil
}

// signingView reads, fresh, what a transaction commits to: the chain's identity,
// and the runtime at the newest finalized block, which starts the era.
func (self *bittensorChainEngine) signingView(ctx context.Context) (*bittensorChainView, error) {
	genesis, err := self.chain.GenesisHash(ctx)
	if err != nil {
		return nil, err
	}
	if genesis != self.pins.genesisHash {
		return nil, errBittensorChainWrongChain
	}
	head, err := self.chain.FinalizedHead(ctx)
	if err != nil {
		return nil, err
	}
	version, err := self.chain.RuntimeVersionAt(ctx, head.Hash)
	if err != nil {
		return nil, err
	}
	meta, err := self.chain.MetadataAt(ctx, head.Hash, version)
	if err != nil {
		return nil, err
	}
	self.cacheMetadata(version, meta)
	return &bittensorChainView{head: head, version: version, meta: meta}, nil
}

var errBittensorChainWrongChain = errors.New("the endpoint serves another chain")

func (self *bittensorChainEngine) escrowHotkeyParams() []any {
	return []any{
		map[string]string{
			"to":   evm.AddressHex(self.pins.vault2),
			"data": "0x" + hex.EncodeToString(evm.PackEscrowHotkey()),
		},
		"latest",
	}
}

// checkEscrowHotkey refuses unless the vault's escrowHotkey() is the pinned one.
func (self *bittensorChainEngine) checkEscrowHotkey(reply json.RawMessage) error {
	var encoded string
	if err := json.Unmarshal(reply, &encoded); err != nil {
		return err
	}
	data, err := evm.ParseHexBytes(encoded)
	if err != nil {
		return err
	}
	if len(data) != 32 || [32]byte(data) != self.pins.escrowHotkey {
		return errBittensorChainWrongChain
	}
	return nil
}

func (self *bittensorChainEngine) signingContext(view *bittensorChainView, nonce uint32) subtensor.SigningContext {
	return subtensor.SigningContext{
		GenesisHash:        self.pins.genesisHash,
		SpecVersion:        view.version.SpecVersion,
		TransactionVersion: view.version.TransactionVersion,
		BirthBlockNumber:   view.head.Number,
		BirthBlockHash:     view.head.Hash,
		Nonce:              nonce,
	}
}

// quoteFee is the fee of a call from the signer, in rao of TAO, with no key.
func (self *bittensorChainEngine) quoteFee(ctx context.Context, view *bittensorChainView, signer [32]byte, call *subtensor.Call) (uint64, error) {
	prepared, err := subtensor.Prepare(view.meta, self.signingContext(view, 0), signer, call)
	if err != nil {
		return 0, err
	}
	return self.chain.QuoteFee(ctx, prepared.UnsignedForFee())
}

// freeBalance is an account's TAO in rao.
func (self *bittensorChainEngine) freeBalance(ctx context.Context, view *bittensorChainView, account [32]byte) (subtensor.AccountBalance, error) {
	return self.chain.ReadAccountBalance(ctx, view.meta, account)
}

// burn is what registering on the pinned subnet burns now, in rao of TAO.
func (self *bittensorChainEngine) burn(ctx context.Context, view *bittensorChainView) (uint64, error) {
	return self.chain.ReadBurn(ctx, view.meta, self.pins.netuid)
}

// A transaction under a fresh view, signed and not sent.
type bittensorSignedCall struct {
	view   *bittensorChainView
	signed *subtensor.Signed
}

// signCall reads what a transaction commits to and signs the call: the runtime
// is checked for the call and its extensions first, and refuses with
// subtensor.RuntimeChangedError rather than sign.
func (self *bittensorChainEngine) signCall(ctx context.Context, signer subtensor.Signer, call *subtensor.Call) (*bittensorSignedCall, error) {
	view, err := self.signingView(ctx)
	if err != nil {
		return nil, err
	}
	nonce, err := self.chain.AccountNonce(ctx, signer.PublicKey())
	if err != nil {
		return nil, err
	}
	prepared, err := subtensor.Prepare(view.meta, self.signingContext(view, nonce), signer.PublicKey(), call)
	if err != nil {
		return nil, err
	}
	signed, err := prepared.Sign(signer)
	if err != nil {
		return nil, err
	}
	return &bittensorSignedCall{view: view, signed: signed}, nil
}

// sendCall sends a signed transaction, says it was sent, and waits for its
// finalized outcome, or for subtensor.ErrExpired when it can no longer land.
func (self *bittensorChainEngine) sendCall(ctx context.Context, call *bittensorSignedCall, sent func(txHash string)) (*subtensor.Outcome, error) {
	if err := self.chain.Submit(ctx, call.signed); err != nil {
		return nil, err
	}
	if sent != nil {
		sent("0x" + hex.EncodeToString(call.signed.Hash[:]))
	}
	first, last := call.landingBlocks()
	return self.waitCall(ctx, call.view.meta, call.signed.Hash, first, last)
}

// landingBlocks are the first and last blocks the transaction can land in.
func (self *bittensorSignedCall) landingBlocks() (uint64, uint64) {
	return self.view.head.Number + 1, self.view.head.Number + subtensor.EraPeriodBlocks
}

// waitCall waits for a sent transaction's finalized outcome.
func (self *bittensorChainEngine) waitCall(ctx context.Context, meta *subtensor.Metadata, hash [32]byte, first uint64, last uint64) (*subtensor.Outcome, error) {
	return self.chain.WaitOutcome(ctx, meta, hash, subtensor.WaitOptions{
		FirstBlock: first,
		LastBlock:  last,
		Poll:       self.poll,
		Sleep:      self.sleep,
	})
}

// submitCall is signCall then sendCall: the one way a transaction leaves the sdk.
func (self *bittensorChainEngine) submitCall(ctx context.Context, signer subtensor.Signer, call *subtensor.Call, sent func(txHash string)) (*subtensor.Outcome, error) {
	signed, err := self.signCall(ctx, signer, call)
	if err != nil {
		return nil, err
	}
	return self.sendCall(ctx, signed, sent)
}

// a * b / c rounded down, for rao amounts and prices. False when it does not
// fit or c is zero.
func bittensorMulDiv(a uint64, b uint64, c uint64) (uint64, bool) {
	if c == 0 {
		return 0, false
	}
	value := new(big.Int).Mul(new(big.Int).SetUint64(a), new(big.Int).SetUint64(b))
	value.Quo(value, new(big.Int).SetUint64(c))
	if !value.IsUint64() {
		return 0, false
	}
	return value.Uint64(), true
}

// a * b / c rounded up.
func bittensorMulDivCeil(a uint64, b uint64, c uint64) (uint64, bool) {
	if c == 0 {
		return 0, false
	}
	value := new(big.Int).Mul(new(big.Int).SetUint64(a), new(big.Int).SetUint64(b))
	divisor := new(big.Int).SetUint64(c)
	value.Add(value, new(big.Int).Sub(divisor, big.NewInt(1)))
	value.Quo(value, divisor)
	if !value.IsUint64() {
		return 0, false
	}
	return value.Uint64(), true
}

// The sdk's own code for a chain that is not the release's.
const BittensorChainErrorWrongChain = "bittensor_wrong_chain"
