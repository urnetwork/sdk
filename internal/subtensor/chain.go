package subtensor

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/blake2b"

	"github.com/urnetwork/sdk/sn/evm"
)

// The reads a transaction needs and its submission, over a chain's JSON-RPC.
// An endpoint is not trusted with anything that decides what gets signed: the
// answers here are checked for shape, and whatever could be a lie can only make
// a transaction invalid or a quote wrong. The caller holds its limits.

// RpcClient is the JSON-RPC transport. evm.Client is one.
type RpcClient interface {
	Call(ctx context.Context, method string, params ...any) (json.RawMessage, error)
	Batch(ctx context.Context, calls []evm.BatchCall) ([]evm.BatchResult, error)
}

// Chain reads and writes one chain.
type Chain struct {
	rpc RpcClient
}

// NewChain reads and writes over the client.
func NewChain(rpc RpcClient) *Chain {
	return &Chain{rpc: rpc}
}

func hexPrefixed(data []byte) string {
	return "0x" + hex.EncodeToString(data)
}

func parseHexBytes(value string) ([]byte, error) {
	trimmed, ok := strings.CutPrefix(value, "0x")
	if !ok {
		return nil, fmt.Errorf("not hex: %.40q", value)
	}
	return hex.DecodeString(trimmed)
}

func parseRawHexBytes(raw json.RawMessage) ([]byte, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return parseHexBytes(value)
}

func parseRawHash(raw json.RawMessage) ([32]byte, error) {
	data, err := parseRawHexBytes(raw)
	if err != nil {
		return [32]byte{}, err
	}
	if len(data) != 32 {
		return [32]byte{}, fmt.Errorf("a hash of %d bytes", len(data))
	}
	return [32]byte(data), nil
}

// GenesisHash is the chain's identity.
func (self *Chain) GenesisHash(ctx context.Context) ([32]byte, error) {
	raw, err := self.rpc.Call(ctx, "chain_getBlockHash", 0)
	if err != nil {
		return [32]byte{}, err
	}
	return parseRawHash(raw)
}

// ReadIdentity reads the chain's genesis hash and current runtime version, and
// one more call for the caller, in one JSON-RPC batch.
func (self *Chain) ReadIdentity(ctx context.Context, extra evm.BatchCall) ([32]byte, RuntimeVersion, json.RawMessage, error) {
	results, err := self.rpc.Batch(ctx, []evm.BatchCall{
		{Method: "chain_getBlockHash", Params: []any{0}},
		{Method: "state_getRuntimeVersion", Params: []any{}},
		extra,
	})
	if err != nil {
		return [32]byte{}, RuntimeVersion{}, nil, err
	}
	if len(results) != 3 {
		return [32]byte{}, RuntimeVersion{}, nil, errors.New("a short batch reply")
	}
	for _, result := range results {
		if result.Err != nil {
			return [32]byte{}, RuntimeVersion{}, nil, result.Err
		}
	}
	genesis, err := parseRawHash(results[0].Result)
	if err != nil {
		return [32]byte{}, RuntimeVersion{}, nil, err
	}
	var version RuntimeVersion
	if err := json.Unmarshal(results[1].Result, &version); err != nil {
		return [32]byte{}, RuntimeVersion{}, nil, err
	}
	return genesis, version, results[2].Result, nil
}

// BlockRef names one block.
type BlockRef struct {
	Hash   [32]byte
	Number uint64
}

// FinalizedHead is the newest finalized block.
func (self *Chain) FinalizedHead(ctx context.Context) (BlockRef, error) {
	raw, err := self.rpc.Call(ctx, "chain_getFinalizedHead")
	if err != nil {
		return BlockRef{}, err
	}
	hash, err := parseRawHash(raw)
	if err != nil {
		return BlockRef{}, err
	}
	raw, err = self.rpc.Call(ctx, "chain_getHeader", hexPrefixed(hash[:]))
	if err != nil {
		return BlockRef{}, err
	}
	var header struct {
		Number string `json:"number"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return BlockRef{}, err
	}
	number, err := strconv.ParseUint(strings.TrimPrefix(header.Number, "0x"), 16, 64)
	if err != nil {
		return BlockRef{}, fmt.Errorf("header number %.40q: %w", header.Number, err)
	}
	return BlockRef{Hash: hash, Number: number}, nil
}

// RuntimeVersion is what a signature commits a transaction to.
type RuntimeVersion struct {
	SpecName           string `json:"specName"`
	SpecVersion        uint32 `json:"specVersion"`
	TransactionVersion uint32 `json:"transactionVersion"`
}

// RuntimeVersionAt reads the runtime version at a block.
func (self *Chain) RuntimeVersionAt(ctx context.Context, block [32]byte) (RuntimeVersion, error) {
	raw, err := self.rpc.Call(ctx, "state_getRuntimeVersion", hexPrefixed(block[:]))
	if err != nil {
		return RuntimeVersion{}, err
	}
	var version RuntimeVersion
	if err := json.Unmarshal(raw, &version); err != nil {
		return RuntimeVersion{}, err
	}
	return version, nil
}

// MetadataAt reads and parses the runtime metadata at a block, and checks it
// says it is the runtime version the block reports.
func (self *Chain) MetadataAt(ctx context.Context, block [32]byte, version RuntimeVersion) (*Metadata, error) {
	raw, err := self.rpc.Call(ctx, "state_getMetadata", hexPrefixed(block[:]))
	if err != nil {
		return nil, err
	}
	return parseMetadataReply(raw, version)
}

// CurrentMetadata reads and parses the best block's runtime metadata, which has
// to be the given runtime version.
func (self *Chain) CurrentMetadata(ctx context.Context, version RuntimeVersion) (*Metadata, error) {
	raw, err := self.rpc.Call(ctx, "state_getMetadata")
	if err != nil {
		return nil, err
	}
	return parseMetadataReply(raw, version)
}

func parseMetadataReply(raw json.RawMessage, version RuntimeVersion) (*Metadata, error) {
	data, err := parseRawHexBytes(raw)
	if err != nil {
		return nil, err
	}
	meta, err := ParseMetadata(data)
	if err != nil {
		return nil, err
	}
	if err := meta.checkVersion(version); err != nil {
		return nil, err
	}
	return meta, nil
}

// checkVersion compares the runtime version built into the metadata with the
// one reported for the block.
func (self *Metadata) checkVersion(version RuntimeVersion) error {
	system := self.pallets["System"]
	if system == nil {
		return runtimeChanged("pallet System is gone")
	}
	constant, ok := system.constants["Version"]
	if !ok {
		return runtimeChanged("constant System.Version is gone")
	}
	value, err := self.decodeValue(constant.value, constant.ty)
	if err != nil {
		return runtimeChanged("constant System.Version: %v", err)
	}
	fields, ok := value.(*valueFields)
	if !ok {
		return runtimeChanged("constant System.Version is not a struct")
	}
	specName, _ := fields.get("spec_name").(string)
	specVersion, specOk := valueUint64(fields.get("spec_version"))
	transactionVersion, transactionOk := valueUint64(fields.get("transaction_version"))
	if !specOk || !transactionOk {
		return runtimeChanged("constant System.Version has no versions")
	}
	if specName != version.SpecName || specVersion != uint64(version.SpecVersion) || transactionVersion != uint64(version.TransactionVersion) {
		return errors.New("the metadata and the runtime version are not the same runtime")
	}
	return nil
}

func (self *Chain) stateCall(ctx context.Context, method string, args []byte) ([]byte, error) {
	raw, err := self.rpc.Call(ctx, "state_call", method, hexPrefixed(args))
	if err != nil {
		return nil, err
	}
	return parseRawHexBytes(raw)
}

// AccountNonce is the account's nonce in the best block's state, not counting
// transactions in the pool. A second transaction while one is pending takes the
// same nonce, and the pool refuses it, so a repeated tap cannot do a thing twice.
func (self *Chain) AccountNonce(ctx context.Context, account [32]byte) (uint32, error) {
	reply, err := self.stateCall(ctx, "AccountNonceApi_account_nonce", account[:])
	if err != nil {
		return 0, err
	}
	if len(reply) != 4 {
		return 0, runtimeChanged("account nonce of %d bytes", len(reply))
	}
	return binary.LittleEndian.Uint32(reply), nil
}

// Stake is the alpha a coldkey holds on a hotkey on a subnet, in rao.
func (self *Chain) Stake(ctx context.Context, hotkey [32]byte, coldkey [32]byte, netuid uint16) (uint64, error) {
	reply, err := self.stateCall(ctx, "StakeInfoRuntimeApi_get_stake_info_for_hotkey_coldkey_netuid", stakeInfoArgs(hotkey, coldkey, netuid))
	if err != nil {
		return 0, err
	}
	return parseStakeInfo(reply, hotkey, coldkey, netuid)
}

func stakeInfoArgs(hotkey [32]byte, coldkey [32]byte, netuid uint16) []byte {
	args := make([]byte, 0, 32+32+2)
	args = append(args, hotkey[:]...)
	args = append(args, coldkey[:]...)
	return binary.LittleEndian.AppendUint16(args, netuid)
}

// parseStakeInfo reads Option<StakeInfo>: hotkey, coldkey, then compact netuid,
// stake, locked, emission, tao_emission and drain, then is_registered.
func parseStakeInfo(reply []byte, hotkey [32]byte, coldkey [32]byte, netuid uint16) (uint64, error) {
	r := newScaleReader(reply)
	present, err := r.readOption()
	if err != nil {
		return 0, runtimeChanged("stake info: %v", err)
	}
	if !present {
		if r.remaining() != 0 {
			return 0, runtimeChanged("stake info has trailing bytes")
		}
		return 0, nil
	}
	gotHotkey, err := r.take(32)
	if err != nil {
		return 0, runtimeChanged("stake info: %v", err)
	}
	gotColdkey, err := r.take(32)
	if err != nil {
		return 0, runtimeChanged("stake info: %v", err)
	}
	if !bytes.Equal(gotHotkey, hotkey[:]) || !bytes.Equal(gotColdkey, coldkey[:]) {
		return 0, runtimeChanged("stake info is for another account")
	}
	var values [6]uint64
	for i := range values {
		values[i], err = r.readCompact()
		if err != nil {
			return 0, runtimeChanged("stake info: %v", err)
		}
	}
	if values[0] != uint64(netuid) {
		return 0, runtimeChanged("stake info is for another subnet")
	}
	if _, err := r.readBool(); err != nil {
		return 0, runtimeChanged("stake info: %v", err)
	}
	if r.remaining() != 0 {
		return 0, runtimeChanged("stake info has trailing bytes")
	}
	return values[1], nil
}

// AlphaPrice is the subnet's current price in rao of TAO per alpha.
func (self *Chain) AlphaPrice(ctx context.Context, netuid uint16) (uint64, error) {
	reply, err := self.stateCall(ctx, "SwapRuntimeApi_current_alpha_price", binary.LittleEndian.AppendUint16(nil, netuid))
	if err != nil {
		return 0, err
	}
	return parseAlphaPrice(reply)
}

func parseAlphaPrice(reply []byte) (uint64, error) {
	if len(reply) != 8 {
		return 0, runtimeChanged("alpha price of %d bytes", len(reply))
	}
	return binary.LittleEndian.Uint64(reply), nil
}

// SwapSimulation is the runtime's simulation of selling alpha for TAO now.
type SwapSimulation struct {
	// the TAO paid out, in rao
	TaoRao uint64
	// the alpha that goes into the pool, in rao
	AlphaRao uint64
	// the alpha the pool's fee takes, in rao
	AlphaFeeRao uint64
	// the TAO lost to the sale's own price impact, in rao
	TaoSlippageRao uint64
}

// SimulateSell runs the runtime's sim_swap_alpha_for_tao.
func (self *Chain) SimulateSell(ctx context.Context, netuid uint16, alphaRao uint64) (SwapSimulation, error) {
	args := binary.LittleEndian.AppendUint16(nil, netuid)
	args = binary.LittleEndian.AppendUint64(args, alphaRao)
	reply, err := self.stateCall(ctx, "SwapRuntimeApi_sim_swap_alpha_for_tao", args)
	if err != nil {
		return SwapSimulation{}, err
	}
	// tao_amount, alpha_amount, tao_fee, alpha_fee, tao_slippage, alpha_slippage
	if len(reply) != 6*8 {
		return SwapSimulation{}, runtimeChanged("swap simulation of %d bytes", len(reply))
	}
	return SwapSimulation{
		TaoRao:         binary.LittleEndian.Uint64(reply[0:8]),
		AlphaRao:       binary.LittleEndian.Uint64(reply[8:16]),
		AlphaFeeRao:    binary.LittleEndian.Uint64(reply[24:32]),
		TaoSlippageRao: binary.LittleEndian.Uint64(reply[32:40]),
	}, nil
}

// QuoteFee is the runtime's fee for a transaction, in rao of TAO. It takes the
// bytes of Signed.Raw or Prepared.UnsignedForFee and reads only.
func (self *Chain) QuoteFee(ctx context.Context, extrinsic []byte) (uint64, error) {
	args := append([]byte(nil), extrinsic...)
	args = binary.LittleEndian.AppendUint32(args, uint32(len(extrinsic)))
	reply, err := self.stateCall(ctx, "TransactionPaymentApi_query_info", args)
	if err != nil {
		return 0, err
	}
	return parseFeeQuote(reply)
}

// parseFeeQuote reads RuntimeDispatchInfo: compact ref_time and proof_size, the
// dispatch class, then the fee as Subtensor's u64 balance.
func parseFeeQuote(reply []byte) (uint64, error) {
	r := newScaleReader(reply)
	for range 2 {
		if _, err := r.readCompact(); err != nil {
			return 0, runtimeChanged("fee quote: %v", err)
		}
	}
	if _, err := r.readU8(); err != nil {
		return 0, runtimeChanged("fee quote: %v", err)
	}
	if r.remaining() != 8 {
		return 0, runtimeChanged("fee quote's fee is %d bytes", r.remaining())
	}
	fee, err := r.take(8)
	if err != nil {
		return 0, runtimeChanged("fee quote: %v", err)
	}
	return binary.LittleEndian.Uint64(fee), nil
}

// Storage keys: twox128(pallet) ‖ twox128(item) ‖ the hashed map key, computed
// once and pinned by test.
const (
	storageSystemAccount             = "26aa394eea5630e07c48ae0c9558cef7b99d880ec681799c0cf30e8886371da9"
	storageSystemEvents              = "26aa394eea5630e07c48ae0c9558cef780d41e5e16056765bc8461851072c9d7"
	storageSubtensorBurn             = "658faa385070e074c85bf6b568cf055501be1755d08418802946bca51b686325"
	storageSubtensorNominatorMinimum = "658faa385070e074c85bf6b568cf0555600828b736cfd977b24a0d2528a8a2e8"

	hasherBlake2_128Concat = 2
	hasherIdentity         = 6
)

func storageKey(prefix string, suffix []byte) string {
	return "0x" + prefix + hex.EncodeToString(suffix)
}

// storageRead is one storage value to read: the rpc call and how to decode it.
// An absent key gives the runtime's declared fallback.
type storageRead struct {
	meta   *Metadata
	label  string
	entry  metadataStorageEntry
	params []any
}

func (self *Metadata) newStorageRead(pallet string, item string, key string, block *[32]byte, hashers []uint8) (*storageRead, error) {
	label := pallet + "." + item
	entry, ok := self.storageEntry(pallet, item)
	if !ok {
		return nil, runtimeChanged("storage %s is gone", label)
	}
	if !bytes.Equal(entry.hashers, hashers) {
		return nil, runtimeChanged("storage %s has other hashers", label)
	}
	params := []any{key}
	if block != nil {
		params = append(params, hexPrefixed(block[:]))
	}
	return &storageRead{meta: self, label: label, entry: entry, params: params}, nil
}

func (self *storageRead) decode(raw json.RawMessage) (any, error) {
	var encoded *string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil, err
	}
	var data []byte
	if encoded != nil {
		var err error
		data, err = parseHexBytes(*encoded)
		if err != nil {
			return nil, err
		}
	} else {
		if self.entry.optional {
			return nil, nil
		}
		data = self.entry.fallback
	}
	value, err := self.meta.decodeValue(data, self.entry.valueType)
	if err != nil {
		return nil, runtimeChanged("storage %s: %v", self.label, err)
	}
	return value, nil
}

func (self *storageRead) batchCall() evm.BatchCall {
	return evm.BatchCall{Method: "state_getStorage", Params: self.params}
}

func (self *Chain) readStorage(ctx context.Context, read *storageRead) (any, error) {
	raw, err := self.rpc.Call(ctx, "state_getStorage", read.params...)
	if err != nil {
		return nil, err
	}
	return read.decode(raw)
}

func (self *Metadata) storageEntry(pallet string, item string) (metadataStorageEntry, bool) {
	entry := self.pallets[pallet]
	if entry == nil {
		return metadataStorageEntry{}, false
	}
	storage, ok := entry.storage[item]
	return storage, ok
}

func accountStorageKey(account [32]byte) string {
	hash, _ := blake2b.New(16, nil)
	hash.Write(account[:])
	return storageKey(storageSystemAccount, append(hash.Sum(nil), account[:]...))
}

// AccountBalance is an account's TAO.
type AccountBalance struct {
	FreeRao     uint64
	ReservedRao uint64
	FrozenRao   uint64
}

func (self *Metadata) accountRead(account [32]byte) (*storageRead, error) {
	return self.newStorageRead("System", "Account", accountStorageKey(account), nil, []uint8{hasherBlake2_128Concat})
}

func parseAccountBalance(value any) (AccountBalance, error) {
	fields, ok := value.(*valueFields)
	if !ok {
		return AccountBalance{}, runtimeChanged("System.Account is not a struct")
	}
	data, ok := fields.get("data").(*valueFields)
	if !ok {
		return AccountBalance{}, runtimeChanged("System.Account has no data")
	}
	free, freeOk := valueUint64(data.get("free"))
	reserved, reservedOk := valueUint64(data.get("reserved"))
	frozen, frozenOk := valueUint64(data.get("frozen"))
	if !freeOk || !reservedOk || !frozenOk {
		return AccountBalance{}, runtimeChanged("System.Account's balances are not u64")
	}
	return AccountBalance{FreeRao: free, ReservedRao: reserved, FrozenRao: frozen}, nil
}

// ReadAccountBalance reads an account's TAO in rao. An account that does not
// exist holds none.
func (self *Chain) ReadAccountBalance(ctx context.Context, meta *Metadata, account [32]byte) (AccountBalance, error) {
	read, err := meta.accountRead(account)
	if err != nil {
		return AccountBalance{}, err
	}
	value, err := self.readStorage(ctx, read)
	if err != nil {
		return AccountBalance{}, err
	}
	return parseAccountBalance(value)
}

// ReadBurn is what registering on the subnet burns now, in rao of TAO.
func (self *Chain) ReadBurn(ctx context.Context, meta *Metadata, netuid uint16) (uint64, error) {
	key := storageKey(storageSubtensorBurn, binary.LittleEndian.AppendUint16(nil, netuid))
	read, err := meta.newStorageRead(palletSubtensorModule, "Burn", key, nil, []uint8{hasherIdentity})
	if err != nil {
		return 0, err
	}
	value, err := self.readStorage(ctx, read)
	if err != nil {
		return 0, err
	}
	burn, ok := valueUint64(value)
	if !ok {
		return 0, runtimeChanged("SubtensorModule.Burn is not an integer")
	}
	return burn, nil
}

func (self *Metadata) nominatorMinimumRead() (*storageRead, error) {
	return self.newStorageRead(palletSubtensorModule, "NominatorMinRequiredStake", storageKey(storageSubtensorNominatorMinimum, nil), nil, nil)
}

// SaleState is what selling alpha depends on, read together.
type SaleState struct {
	// the alpha the coldkey holds on the hotkey, in rao
	StakeRao uint64
	Balance  AccountBalance
	// rao of TAO per alpha
	PriceRao uint64
	// the least TAO value a nomination keeps, in rao. Stake left below it on a
	// hotkey the coldkey does not own is unstaked with the sale that left it.
	NominatorMinimumRao uint64
}

// ReadSaleState reads a sale's state in one JSON-RPC batch, one request against
// an endpoint's rate limit.
func (self *Chain) ReadSaleState(ctx context.Context, meta *Metadata, hotkey [32]byte, coldkey [32]byte, netuid uint16) (SaleState, error) {
	accountRead, err := meta.accountRead(coldkey)
	if err != nil {
		return SaleState{}, err
	}
	minimumRead, err := meta.nominatorMinimumRead()
	if err != nil {
		return SaleState{}, err
	}
	netuidArg := binary.LittleEndian.AppendUint16(nil, netuid)
	results, err := self.rpc.Batch(ctx, []evm.BatchCall{
		stateCallBatch("StakeInfoRuntimeApi_get_stake_info_for_hotkey_coldkey_netuid", stakeInfoArgs(hotkey, coldkey, netuid)),
		accountRead.batchCall(),
		stateCallBatch("SwapRuntimeApi_current_alpha_price", netuidArg),
		minimumRead.batchCall(),
	})
	if err != nil {
		return SaleState{}, err
	}
	if len(results) != 4 {
		return SaleState{}, errors.New("a short batch reply")
	}
	for _, result := range results {
		if result.Err != nil {
			return SaleState{}, result.Err
		}
	}
	state := SaleState{}
	stakeReply, err := parseRawHexBytes(results[0].Result)
	if err != nil {
		return SaleState{}, err
	}
	state.StakeRao, err = parseStakeInfo(stakeReply, hotkey, coldkey, netuid)
	if err != nil {
		return SaleState{}, err
	}
	accountValue, err := accountRead.decode(results[1].Result)
	if err != nil {
		return SaleState{}, err
	}
	state.Balance, err = parseAccountBalance(accountValue)
	if err != nil {
		return SaleState{}, err
	}
	priceReply, err := parseRawHexBytes(results[2].Result)
	if err != nil {
		return SaleState{}, err
	}
	state.PriceRao, err = parseAlphaPrice(priceReply)
	if err != nil {
		return SaleState{}, err
	}
	minimumValue, err := minimumRead.decode(results[3].Result)
	if err != nil {
		return SaleState{}, err
	}
	minimum, ok := valueUint64(minimumValue)
	if !ok {
		return SaleState{}, runtimeChanged("SubtensorModule.NominatorMinRequiredStake is not an integer")
	}
	state.NominatorMinimumRao = minimum
	return state, nil
}

func stateCallBatch(method string, args []byte) evm.BatchCall {
	return evm.BatchCall{Method: "state_call", Params: []any{method, hexPrefixed(args)}}
}

// SubmitRefusal is the pool's refusal of a transaction. Nothing was charged.
type SubmitRefusal struct {
	Code    int
	Message string
	Data    string
}

func (self *SubmitRefusal) Error() string {
	return fmt.Sprintf("refused %d: %s (%s)", self.Code, self.Message, self.Data)
}

// Submit sends a signed transaction, and checks the node names it by the same
// hash. A pool's refusal comes back as a SubmitRefusal.
func (self *Chain) Submit(ctx context.Context, signed *Signed) error {
	raw, err := self.rpc.Call(ctx, "author_submitExtrinsic", hexPrefixed(signed.Raw))
	if err != nil {
		var rpcErr *evm.RpcError
		if errors.As(err, &rpcErr) {
			return &SubmitRefusal{Code: rpcErr.Code, Message: rpcErr.Message, Data: string(rpcErr.Data)}
		}
		return err
	}
	hash, err := parseRawHash(raw)
	if err != nil {
		return err
	}
	if hash != signed.Hash {
		return errors.New("the node names this transaction by another hash")
	}
	return nil
}

// StakeSale is one SubtensorModule.StakeRemoved of a transaction.
type StakeSale struct {
	Coldkey [32]byte
	Hotkey  [32]byte
	// the TAO paid out, in rao
	TaoRao uint64
	// the alpha that went into the pool, in rao
	AlphaRao uint64
	Netuid   uint64
	// the alpha the pool's fee took, in rao
	PoolFeeAlphaRao uint64
}

// AlphaFeePayment is SubtensorModule.TransactionFeePaidWithAlpha.
type AlphaFeePayment struct {
	AlphaRao uint64
	TaoRao   uint64
}

// Outcome is what a transaction did, from its finalized block's events.
type Outcome struct {
	Block          BlockRef
	ExtrinsicIndex uint32
	Success        bool
	// "Pallet.Error" for a failed call, else the kind of failure
	Failure string
	Sales   []StakeSale
	// nil when the fee was not paid in alpha
	AlphaFee *AlphaFeePayment
	// the fee when paid in TAO, in rao
	TaoFeeRao uint64
}

// ErrExpired says a transaction's era ended and it never landed. It never will.
var ErrExpired = errors.New("the transaction's era ended before it landed")

// WaitOptions says where to look for a transaction.
type WaitOptions struct {
	// the first and last blocks it could land in
	FirstBlock uint64
	LastBlock  uint64
	Poll       time.Duration
	// waits; the tests inject it
	Sleep func(ctx context.Context, wait time.Duration) error
}

// WaitOutcome walks finalized blocks for the transaction and reads its events.
// It returns ErrExpired once the last block it could land in is finalized
// without it, and the context's error if the chain cannot be read until then.
func (self *Chain) WaitOutcome(ctx context.Context, meta *Metadata, hash [32]byte, options WaitOptions) (*Outcome, error) {
	next := options.FirstBlock
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		head, err := self.FinalizedHead(ctx)
		if err == nil {
			for next <= head.Number && next <= options.LastBlock {
				outcome, err := self.findInBlock(ctx, meta, hash, next)
				if errors.Is(err, ErrOutcomeUnreadable) {
					return nil, err
				}
				if err != nil {
					// the endpoint failed: this block again next time
					break
				}
				if outcome != nil {
					return outcome, nil
				}
				next += 1
			}
			if options.LastBlock < next {
				return nil, ErrExpired
			}
		}
		if err := options.Sleep(ctx, options.Poll); err != nil {
			return nil, err
		}
	}
}

func (self *Chain) findInBlock(ctx context.Context, meta *Metadata, hash [32]byte, number uint64) (*Outcome, error) {
	raw, err := self.rpc.Call(ctx, "chain_getBlockHash", number)
	if err != nil {
		return nil, err
	}
	blockHash, err := parseRawHash(raw)
	if err != nil {
		return nil, err
	}
	raw, err = self.rpc.Call(ctx, "chain_getBlock", hexPrefixed(blockHash[:]))
	if err != nil {
		return nil, err
	}
	var reply struct {
		Block struct {
			Extrinsics []string `json:"extrinsics"`
		} `json:"block"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, err
	}
	for i, extrinsic := range reply.Block.Extrinsics {
		data, err := parseHexBytes(extrinsic)
		if err != nil {
			return nil, err
		}
		if blake2b.Sum256(data) != hash {
			continue
		}
		block := BlockRef{Hash: blockHash, Number: number}
		read, err := meta.newStorageRead("System", "Events", storageKey(storageSystemEvents, nil), &block.Hash, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrOutcomeUnreadable, err)
		}
		raw, err := self.rpc.Call(ctx, "state_getStorage", read.params...)
		if err != nil {
			return nil, err
		}
		value, err := read.decode(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrOutcomeUnreadable, err)
		}
		outcome, err := meta.outcomeFromEvents(value, uint64(i))
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrOutcomeUnreadable, err)
		}
		outcome.Block = block
		outcome.ExtrinsicIndex = uint32(i)
		return outcome, nil
	}
	return nil, nil
}

// ErrOutcomeUnreadable says a transaction landed and its events could not be
// read, so what it did is not known from here.
var ErrOutcomeUnreadable = errors.New("the transaction landed and its events could not be read")

func (self *Metadata) outcomeFromEvents(value any, index uint64) (*Outcome, error) {
	records, ok := value.([]any)
	if !ok {
		return nil, errors.New("System.Events is not a list")
	}
	outcome := &Outcome{}
	found := false
	for _, record := range records {
		fields, ok := record.(*valueFields)
		if !ok {
			return nil, errors.New("an event record is not a struct")
		}
		phase, ok := fields.get("phase").(*valueVariant)
		if !ok {
			return nil, errors.New("an event record has no phase")
		}
		applied, _ := valueUint64(phase.fields.at(0))
		if phase.name != "ApplyExtrinsic" || applied != index {
			continue
		}
		palletEvent, ok := fields.get("event").(*valueVariant)
		if !ok {
			return nil, errors.New("an event record has no event")
		}
		event, ok := palletEvent.fields.at(0).(*valueVariant)
		if !ok {
			continue
		}
		switch palletEvent.name + "." + event.name {
		case "System.ExtrinsicSuccess":
			outcome.Success, found = true, true
		case "System.ExtrinsicFailed":
			outcome.Failure, found = self.dispatchFailure(event.fields.get("dispatch_error")), true
		case "SubtensorModule.StakeRemoved":
			sale, ok := parseStakeRemoved(event.fields)
			if !ok {
				return nil, errors.New("StakeRemoved has another shape")
			}
			outcome.Sales = append(outcome.Sales, sale)
		case "SubtensorModule.TransactionFeePaidWithAlpha":
			alpha, alphaOk := valueUint64(event.fields.get("alpha_fee"))
			tao, taoOk := valueUint64(event.fields.get("tao_amount"))
			if !alphaOk || !taoOk {
				return nil, errors.New("TransactionFeePaidWithAlpha has another shape")
			}
			outcome.AlphaFee = &AlphaFeePayment{AlphaRao: alpha, TaoRao: tao}
		case "TransactionPayment.TransactionFeePaid":
			fee, ok := valueUint64(event.fields.get("actual_fee"))
			if !ok {
				return nil, errors.New("TransactionFeePaid has another shape")
			}
			outcome.TaoFeeRao = fee
		}
	}
	if !found {
		return nil, errors.New("the transaction has neither a success nor a failure event")
	}
	return outcome, nil
}

func parseStakeRemoved(fields *valueFields) (StakeSale, bool) {
	coldkey, coldkeyOk := valueBytes32(fields.at(0))
	hotkey, hotkeyOk := valueBytes32(fields.at(1))
	tao, taoOk := valueUint64(fields.at(2))
	alpha, alphaOk := valueUint64(fields.at(3))
	netuid, netuidOk := valueUint64(fields.at(4))
	poolFee, poolFeeOk := valueUint64(fields.at(5))
	if !coldkeyOk || !hotkeyOk || !taoOk || !alphaOk || !netuidOk || !poolFeeOk {
		return StakeSale{}, false
	}
	return StakeSale{Coldkey: coldkey, Hotkey: hotkey, TaoRao: tao, AlphaRao: alpha, Netuid: netuid, PoolFeeAlphaRao: poolFee}, true
}

// dispatchFailure names a DispatchError: "Pallet.Error" for a pallet's error.
func (self *Metadata) dispatchFailure(value any) string {
	failure, ok := value.(*valueVariant)
	if !ok {
		return "unknown"
	}
	if failure.name != "Module" {
		return failure.name
	}
	module, ok := failure.fields.at(0).(*valueFields)
	if !ok {
		return "Module"
	}
	index, indexOk := valueUint64(module.get("index"))
	code, codeOk := module.get("error").([]byte)
	if !indexOk || !codeOk || len(code) == 0 || 255 < index {
		return "Module"
	}
	pallet := self.palletsByIndex[uint8(index)]
	if pallet == nil || !pallet.hasErrors {
		return fmt.Sprintf("Module %d", index)
	}
	errors_, err := self.lookup(pallet.errors)
	if err != nil {
		return pallet.name
	}
	variant := errors_.variantByIndex(code[0])
	if variant == nil {
		return fmt.Sprintf("%s %d", pallet.name, code[0])
	}
	return pallet.name + "." + variant.name
}
