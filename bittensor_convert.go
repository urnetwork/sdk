//go:build !ios_extension && !js

package sdk

// Convert SN25α to TAO, on the same key (apple BTEASY.md). A sheet collects an
// amount and draws what comes back; everything else is decided here: which
// amounts are possible, what to expect, the price limit, how much is just
// enough for a burn, and what to tell the user.
//
// The one transaction is SubtensorModule.remove_stake_limit on the pinned
// subnet and the vault's escrow hotkey, all of the amount or none of it, with a
// price limit. The chain pays the TAO to the coldkey that signs, so a wrong
// answer from an endpoint cannot send anything elsewhere. The device builds it,
// signs it and sends it straight to the chain: URnetwork pays nothing and is not
// in the path.
//
// What the runtime does, from Subtensor's source at spec 486, and checked by the
// testnet steps in internal/subtensor/TESTNET.md:
//   - A coldkey with no TAO can send this call, and the fee comes out of the
//     alpha on the same hotkey, before the call runs. So a key with no TAO cannot
//     sell everything: selling what the fee already took fails and the fee stays
//     charged. MaxAmountRao leaves room for it.
//   - A sale worth less than the runtime's minimum stake (0.002 TAO) is refused,
//     unless it empties the position, which only a key that pays its fee in TAO
//     can aim for exactly. MinAmountRao is that floor.
//   - Stake left under the nominator minimum (0.02 TAO of value) is sold with the
//     sale that left it, at the market price. RemainderSweptAlphaRao says so.
//   - A call that fails after its fee was charged keeps the fee.
//
// No phrase reaches an error, a log or anything but the signature.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk/internal/subtensor"
	"github.com/urnetwork/sdk/sn/evm"
	"github.com/urnetwork/sdk/sn/ss58"
)

const (
	bittensorBasisPoints = 10_000

	// how far the price may move between the estimate and the sale
	bittensorConvertSlippageBps uint64 = 200
	// A sale moves the pool's price further than its own average slippage:
	// about twice for an even pool. The limit allows three times, on top of
	// bittensorConvertSlippageBps, so a sale of any accepted size can complete.
	bittensorConvertImpactLimitFactor uint64 = 3
	// above this a sale moves the price too much to do at once
	bittensorConvertMaxImpactBps uint64 = 500
	// a limit further than this under the price was not made by a recent estimate
	bittensorConvertLimitSanityBps uint64 = 3_000

	bittensorConvertEstimateTimeout = 60 * time.Second
	bittensorConvertSubmitTimeout   = 2 * time.Minute
	// reads a typed amount does not repeat
	bittensorConvertStateTtl = 12 * time.Second

	// what a burn is allowed to grow, and the fee with it, between this estimate
	// and the registration
	bittensorConvertBurnMarginBps uint64 = 1_000
)

// bittensorConvertFundsDisclaimer is the id of the funds disclaimer every easy flow
// shows (owner, 2026-11-03), and every convert result a sheet draws carries it.
// An app shows the text it has under this id in the user's language. Convert only
// runs with the key in the app's secure storage, so it is the full disclaimer, the
// owner's three sentences. The id is the key flow's: once its
// BittensorFundsDisclaimerFull is on main, convert takes that instead of the literal.
const bittensorConvertFundsDisclaimer = "bittensor_funds_disclaimer"

// newBittensorChainEngineForExport is how the exported calls get their engine:
// the settings' endpoint, under the release's pins. Tests answer from a fake chain.
var newBittensorChainEngineForExport = newBittensorChainEngine

// Stable codes in SnError.Code, so the apps can localize them. Each comes with a
// message fit to show as it is.
const (
	// the runtime's call or transaction format is not the one this sdk knows:
	// nothing was signed, and an app update is what fixes it
	BittensorConvertErrorRuntimeChanged = "convert_runtime_changed"
	// there is no SN25α on this key to convert
	BittensorConvertErrorNothingToConvert = "convert_nothing_to_convert"
	// the key holds too little to convert any of it
	BittensorConvertErrorBalanceTooLow = "convert_balance_too_low"
	// the amount is under MinAmountRao
	BittensorConvertErrorAmountTooLow = "convert_amount_too_low"
	// the amount is over MaxAmountRao
	BittensorConvertErrorAmountTooHigh = "convert_amount_too_high"
	// the sale would move the price too much at once
	BittensorConvertErrorPriceImpact = "convert_price_impact_too_high"
	// the price moved past the limit: estimate again
	BittensorConvertErrorPriceMoved = "convert_price_moved"
	// the phrase does not open the estimate's key
	BittensorConvertErrorKeyMismatch = "convert_key_mismatch"
	// another conversion from this key has not landed yet
	BittensorConvertErrorPending = "convert_already_pending"
	// the network refused the transaction and nothing was charged
	BittensorConvertErrorRefused = "convert_refused"
	// the transaction did not land in time and now cannot: nothing was charged
	BittensorConvertErrorExpired = "convert_expired"
	// the transaction landed and the sale failed, and the fee stays charged
	BittensorConvertErrorFailed = "convert_failed"
	// the transaction was sent and what it did could not be read: check the balance
	BittensorConvertErrorOutcomeUnknown = "convert_outcome_unknown"
)

// BittensorConvertEstimateResult is everything a sheet shows for an amount, and
// what it hands back to BittensorConvertSubmit. All amounts are in rao, 1e-9 of
// a unit. When Error is set, CanConvert is false and the fields that could be
// read are filled in, so a sheet can still draw the limits.
type BittensorConvertEstimateResult struct {
	ColdkeySs58 string `json:"coldkey_ss58"`
	// the vault's escrow hotkey, which holds this key's SN25α
	HotkeySs58 string `json:"hotkey_ss58,omitempty"`
	Netuid     int64  `json:"netuid"`
	// the SN25α this key holds
	StakeRao int64 `json:"stake_rao"`
	// the TAO this key holds
	FreeTaoRao int64 `json:"free_tao_rao"`
	// rao of TAO per SN25α now
	PriceRao int64 `json:"price_rao"`

	// the amounts that can be converted now; 0 for both when none can
	MinAmountRao int64 `json:"min_amount_rao"`
	MaxAmountRao int64 `json:"max_amount_rao"`

	// the SN25α to sell; 0 asks for the limits alone
	AmountRao int64 `json:"amount_rao"`
	// the TAO the sale should pay, with any remainder swept along
	ExpectedTaoRao int64 `json:"expected_tao_rao"`
	// what the key's TAO should go up by: ExpectedTaoRao less a fee paid in TAO
	ExpectedNetTaoRao int64 `json:"expected_net_tao_rao"`
	// the sale refuses rather than pay less than this for the amount
	MinTaoRao int64 `json:"min_tao_rao"`
	// the sale refuses to go below this price
	LimitPriceRao int64 `json:"limit_price_rao"`
	// what the pool's own fee takes of the amount, in SN25α
	PoolFeeAlphaRao int64 `json:"pool_fee_alpha_rao"`
	// how far the sale itself moves its average price, in basis points
	PriceImpactBps int64 `json:"price_impact_bps"`

	// the network fee, priced in TAO
	FeeTaoRao int64 `json:"fee_tao_rao"`
	// the network fee when it is paid in SN25α
	FeeAlphaRao int64 `json:"fee_alpha_rao"`
	// the key has too little TAO for the fee, so the fee comes out of its SN25α
	FeeInAlpha bool `json:"fee_in_alpha"`

	// stake the sale would leave under the network's minimum, and sold with it
	RemainderSweptAlphaRao int64 `json:"remainder_swept_alpha_rao"`
	RemainderSweptTaoRao   int64 `json:"remainder_swept_tao_rao"`

	// for an estimate made for a burn: the burn now, and the fee to register
	BurnRao        int64 `json:"burn_rao,omitempty"`
	RegisterFeeRao int64 `json:"register_fee_rao,omitempty"`
	// the TAO the registration needs the key to hold
	BurnTaoNeededRao int64 `json:"burn_tao_needed_rao,omitempty"`
	// the key already holds enough, and there is nothing to convert
	BurnAlreadyCovered bool `json:"burn_already_covered,omitempty"`

	// BittensorConvertSubmit can run with this estimate
	CanConvert bool `json:"can_convert"`
	// what the user should know before going ahead, fit to show as it is; "" for nothing
	Notice string `json:"notice,omitempty"`
	// the id of the disclaimer every screen that draws this shows, in the app's strings
	FundsDisclaimer string   `json:"funds_disclaimer"`
	Error           *SnError `json:"error,omitempty"`
}

// BittensorConvertEstimateCallback receives one estimate.
type BittensorConvertEstimateCallback connect.ApiCallback[*BittensorConvertEstimateResult]

// BittensorConvertSubmission says a conversion was sent and where to look for
// what it did. It holds nothing secret, so an app can keep it and call
// BittensorConvertWait again after it was away. When Error is set nothing was sent,
// unless TxHash is set too: then it may have been, and only waiting can tell.
type BittensorConvertSubmission struct {
	TxHash      string `json:"tx_hash,omitempty"`
	ColdkeySs58 string `json:"coldkey_ss58,omitempty"`
	// the first and last blocks it can land in; after the last it never can
	FirstBlock int64 `json:"first_block,omitempty"`
	LastBlock  int64 `json:"last_block,omitempty"`
	// the id of the disclaimer every screen that draws this shows, in the app's strings
	FundsDisclaimer string   `json:"funds_disclaimer"`
	Error           *SnError `json:"error,omitempty"`
}

// BittensorConvertSubmitCallback receives one submission.
type BittensorConvertSubmitCallback connect.ApiCallback[*BittensorConvertSubmission]

// BittensorConvertReceipt is what a finalized conversion did, in rao. When Error
// is set the sale did not happen; a block number says the fee was still charged.
type BittensorConvertReceipt struct {
	TxHash         string `json:"tx_hash,omitempty"`
	BlockNumber    int64  `json:"block_number,omitempty"`
	ExtrinsicIndex int64  `json:"extrinsic_index,omitempty"`
	// the SN25α sold, with a remainder swept along and the pool's fee
	AlphaSoldRao int64 `json:"alpha_sold_rao"`
	// the TAO that reached the key, before a fee paid in TAO
	TaoReceivedRao int64 `json:"tao_received_rao"`
	// the network fee, in SN25α or in TAO, whichever paid it
	FeeAlphaRao int64 `json:"fee_alpha_rao"`
	FeeTaoRao   int64 `json:"fee_tao_rao"`
	// the id of the disclaimer every screen that draws this shows, in the app's strings
	FundsDisclaimer string   `json:"funds_disclaimer"`
	Error           *SnError `json:"error,omitempty"`
}

// BittensorConvertReceiptCallback receives one receipt.
type BittensorConvertReceiptCallback connect.ApiCallback[*BittensorConvertReceipt]

// BittensorConvertEstimate says what converting the amount of SN25α would do
// for the key at the address. An amount of 0 gives the limits alone. It holds no
// key and changes nothing.
func BittensorConvertEstimate(settings *SnChainSettings, coldkeySs58 string, amountRao int64, callback BittensorConvertEstimateCallback) {
	go connect.HandleError(func() {
		ctx, cancel := context.WithTimeout(context.Background(), bittensorConvertEstimateTimeout)
		defer cancel()
		result := bittensorConvertEstimateFor(ctx, settings, coldkeySs58, func(engine *bittensorChainEngine, coldkey [32]byte) *BittensorConvertEstimateResult {
			return engine.convertEstimate(ctx, coldkey, amountRao)
		})
		if callback != nil {
			callback.Result(result, nil)
		}
	})
}

// BittensorConvertEstimateForBurn is the estimate for converting just enough
// SN25α to register a hotkey on the subnet: the burn, its fee and what the key
// must keep, with room for the burn to grow. When the key already holds enough,
// BurnAlreadyCovered is set and there is nothing to convert.
func BittensorConvertEstimateForBurn(settings *SnChainSettings, coldkeySs58 string, callback BittensorConvertEstimateCallback) {
	go connect.HandleError(func() {
		ctx, cancel := context.WithTimeout(context.Background(), bittensorConvertEstimateTimeout)
		defer cancel()
		result := bittensorConvertEstimateFor(ctx, settings, coldkeySs58, func(engine *bittensorChainEngine, coldkey [32]byte) *BittensorConvertEstimateResult {
			return engine.convertEstimateForBurn(ctx, coldkey)
		})
		if callback != nil {
			callback.Result(result, nil)
		}
	})
}

// bittensorConvertEstimateFor is every exported estimate, and every result it
// gives carries the disclaimer.
func bittensorConvertEstimateFor(
	ctx context.Context,
	settings *SnChainSettings,
	coldkeySs58 string,
	estimate func(engine *bittensorChainEngine, coldkey [32]byte) *BittensorConvertEstimateResult,
) *BittensorConvertEstimateResult {
	result := func() *BittensorConvertEstimateResult {
		coldkeySs58 = strings.TrimSpace(coldkeySs58)
		coldkey, err := snColdkeyBytes(coldkeySs58)
		if err != nil {
			return &BittensorConvertEstimateResult{ColdkeySs58: coldkeySs58, Error: newSnError(SnErrorCodeInvalidAddress, "This is not a Bittensor address.")}
		}
		engine, snErr := newBittensorChainEngineForExport(settings)
		if snErr != nil {
			return &BittensorConvertEstimateResult{ColdkeySs58: coldkeySs58, Error: snErr}
		}
		return estimate(engine, coldkey)
	}()
	result.FundsDisclaimer = bittensorConvertFundsDisclaimer
	return result
}

// FormatTaoAmount renders rao as TAO with six decimals: "0.000500". Convert's
// amounts are small, and four decimals would round a burn away.
func FormatTaoAmount(rao int64) string {
	sign := ""
	if rao < 0 {
		sign = "-"
		rao = -rao
	}
	whole := rao / SnRaoPerAlpha
	millionths := (rao%SnRaoPerAlpha + 500) / 1_000
	if millionths == 1_000_000 {
		whole += 1
		millionths = 0
	}
	return fmt.Sprintf("%s%d.%06d", sign, whole, millionths)
}

// FormatTao renders rao with the symbol: "0.000500 TAO".
func FormatTao(rao int64) string {
	return FormatTaoAmount(rao) + " TAO"
}

// All a sale depends on that does not depend on the amount.
type bittensorConvertState struct {
	view    *bittensorChainView
	hotkey  [32]byte
	coldkey [32]byte
	sale    subtensor.SaleState
	feeRao  uint64
	// the existential deposit and the runtime's minimum stake
	depositRao  uint64
	minStakeRao uint64

	feeInAlpha  bool
	feeAlphaRao uint64
	// the alpha set aside for the fee, at the lowest price a sale can run at
	feeReserveRao uint64
	minAmountRao  uint64
	maxAmountRao  uint64
}

// floorRao is the least a sale may pay unless it empties the position.
func (self *bittensorConvertState) floorRao() uint64 {
	return max(self.minStakeRao, self.depositRao)
}

// One state, so typing an amount does not read it all again.
var bittensorConvertStateCache struct {
	sync.Mutex
	key   string
	at    time.Time
	state *bittensorConvertState
}

func (self *bittensorChainEngine) convertState(ctx context.Context, coldkey [32]byte, cached bool) (*bittensorConvertState, error) {
	cacheKey := ""
	if cached && self.cacheKey != "" {
		cacheKey = self.cacheKey + "\n" + string(coldkey[:])
		bittensorConvertStateCache.Lock()
		state := bittensorConvertStateCache.state
		fresh := bittensorConvertStateCache.key == cacheKey && self.now().Sub(bittensorConvertStateCache.at) < bittensorConvertStateTtl
		bittensorConvertStateCache.Unlock()
		if state != nil && fresh {
			return state, nil
		}
	}
	view, hotkey, err := self.quoteView(ctx)
	if err != nil {
		return nil, err
	}
	sale, err := self.chain.ReadSaleState(ctx, view.meta, hotkey, coldkey, self.pins.netuid)
	if err != nil {
		return nil, err
	}
	if sale.PriceRao == 0 {
		return nil, errBittensorConvertNoPrice
	}
	deposit, err := view.meta.ExistentialDeposit()
	if err != nil {
		return nil, err
	}
	minStake, err := view.meta.MinStakeRao()
	if err != nil {
		return nil, err
	}
	// any amount and limit quote the same: their encodings have a fixed size
	call, err := subtensor.NewRemoveStakeLimit(hotkey, self.pins.netuid, 1, 1)
	if err != nil {
		return nil, err
	}
	fee, err := self.quoteFee(ctx, view, coldkey, call)
	if err != nil {
		return nil, err
	}
	state := &bittensorConvertState{
		view:        view,
		hotkey:      hotkey,
		coldkey:     coldkey,
		sale:        sale,
		feeRao:      fee,
		depositRao:  deposit,
		minStakeRao: minStake,
	}
	if err := self.convertLimits(ctx, state); err != nil {
		return nil, err
	}
	if cacheKey != "" {
		bittensorConvertStateCache.Lock()
		bittensorConvertStateCache.key = cacheKey
		bittensorConvertStateCache.at = self.now()
		bittensorConvertStateCache.state = state
		bittensorConvertStateCache.Unlock()
	}
	return state, nil
}

// forgetConvertState drops the kept reads: a conversion changed them.
func forgetConvertState() {
	bittensorConvertStateCache.Lock()
	defer bittensorConvertStateCache.Unlock()
	bittensorConvertStateCache.state = nil
	bittensorConvertStateCache.key = ""
}

var errBittensorConvertNoPrice = errors.New("the subnet has no price")

// convertLimits works out how the fee is paid and which amounts are possible.
func (self *bittensorChainEngine) convertLimits(ctx context.Context, state *bittensorConvertState) error {
	sale := state.sale
	// the runtime takes a fee in TAO only if the key keeps its existential
	// deposit and any frozen balance, and otherwise tries the alpha
	keep := max(state.depositRao, sale.Balance.FrozenRao)
	state.feeInAlpha = sale.Balance.FreeRao < keep || sale.Balance.FreeRao-keep < state.feeRao
	var ok bool
	state.feeAlphaRao, ok = bittensorMulDivCeil(state.feeRao, uint64(SnRaoPerAlpha), sale.PriceRao)
	if !ok {
		return errBittensorConvertNoPrice
	}
	state.maxAmountRao = sale.StakeRao
	if state.feeInAlpha {
		lowest, ok := bittensorConvertLimit(sale.PriceRao, bittensorConvertMaxImpactBps)
		if !ok {
			return errBittensorConvertNoPrice
		}
		state.feeReserveRao, ok = bittensorMulDivCeil(state.feeRao, uint64(SnRaoPerAlpha), lowest)
		if !ok {
			return errBittensorConvertNoPrice
		}
		if sale.StakeRao <= state.feeReserveRao {
			state.maxAmountRao = 0
		} else {
			state.maxAmountRao = sale.StakeRao - state.feeReserveRao
		}
	}
	if state.maxAmountRao == 0 {
		return nil
	}
	// The runtime's floor: a sale paying at least the minimum stake, and at
	// least the deposit for a key that has none yet. It runs when the sale lands,
	// after the price has moved, and a refusal then keeps the fee, so the floor
	// has to hold at the sale's price limit.
	minimum, err := self.convertAmountPaying(ctx, state.floorRao(), sale.PriceRao)
	if err != nil {
		return err
	}
	state.minAmountRao = minimum
	if state.maxAmountRao < state.minAmountRao {
		// a key that pays in TAO can still empty the position, which the
		// runtime's floor does not apply to
		if !state.feeInAlpha && 0 < sale.StakeRao {
			state.minAmountRao = sale.StakeRao
			return nil
		}
		state.minAmountRao, state.maxAmountRao = 0, 0
	}
	return nil
}

// bittensorConvertLimit is the price limit for a sale that moves its own average
// price by the impact, in basis points. The price may also move by
// bittensorConvertSlippageBps before the sale lands.
func bittensorConvertLimit(priceRao uint64, impactBps uint64) (uint64, bool) {
	allowance := bittensorConvertSlippageBps + bittensorConvertImpactLimitFactor*impactBps
	if bittensorConvertMaxImpactBps < impactBps || bittensorBasisPoints <= allowance {
		return 0, false
	}
	limit, ok := bittensorMulDiv(priceRao, bittensorBasisPoints-allowance, bittensorBasisPoints)
	return limit, ok && limit != 0
}

// bittensorConvertSaleImpact is how far a sale moves its own average price, in
// basis points, from the runtime's simulation of it.
func bittensorConvertSaleImpact(simulation subtensor.SwapSimulation) (uint64, bool) {
	return bittensorMulDivCeil(simulation.TaoSlippageRao, bittensorBasisPoints, simulation.TaoRao+simulation.TaoSlippageRao)
}

// bittensorConvertPromise is the least TAO a sale pays if it runs at all: every
// alpha that goes into the pool sells at or above the sale's own limit. The
// runtime's floors are checked against what the sale pays when it runs, so the
// amounts here are sized by this.
func bittensorConvertPromise(priceRao uint64, simulation subtensor.SwapSimulation) (promise uint64, limit uint64, impactBps uint64, err error) {
	impactBps, ok := bittensorConvertSaleImpact(simulation)
	if !ok {
		return 0, 0, 0, errBittensorConvertNoPrice
	}
	if bittensorConvertMaxImpactBps < impactBps {
		return 0, 0, impactBps, errBittensorConvertPoolTooShallow
	}
	limit, ok = bittensorConvertLimit(priceRao, impactBps)
	if !ok {
		return 0, 0, impactBps, errBittensorConvertNoPrice
	}
	promise, ok = bittensorMulDiv(simulation.AlphaRao, limit, uint64(SnRaoPerAlpha))
	if !ok {
		return 0, 0, impactBps, errBittensorConvertNoPrice
	}
	return promise, limit, impactBps, nil
}

var errBittensorConvertPoolTooShallow = errors.New("the pool moves too far for this sale")

// convertAmountPaying is the least alpha whose sale promises at least the TAO at
// today's price. A sale pays close to in proportion to its amount, so each try
// scales the amount by how far the last one fell short.
func (self *bittensorChainEngine) convertAmountPaying(ctx context.Context, taoRao uint64, priceRao uint64) (uint64, error) {
	amount, ok := bittensorMulDivCeil(taoRao, uint64(SnRaoPerAlpha), priceRao)
	if !ok {
		return 0, errBittensorConvertNoPrice
	}
	for range 8 {
		simulation, err := self.chain.SimulateSell(ctx, self.pins.netuid, amount)
		if err != nil {
			return 0, err
		}
		// the runtime's simulation answers zeros when it cannot run a sale
		if simulation.TaoRao == 0 {
			return 0, errBittensorConvertNoPrice
		}
		paid, _, _, err := bittensorConvertPromise(priceRao, simulation)
		if err != nil {
			return 0, err
		}
		if taoRao <= paid {
			return amount, nil
		}
		if paid == 0 {
			return 0, errBittensorConvertNoPrice
		}
		next, ok := bittensorMulDivCeil(amount, taoRao, paid)
		if !ok {
			return 0, errBittensorConvertNoPrice
		}
		amount = max(next, amount) + 1
	}
	return 0, errBittensorConvertNoPrice
}

func (self *bittensorConvertState) newResult(hotkeySs58 string, netuid uint16) *BittensorConvertEstimateResult {
	coldkeySs58, _ := ss58.Encode(self.coldkey, SnSs58Prefix)
	return &BittensorConvertEstimateResult{
		ColdkeySs58:  coldkeySs58,
		HotkeySs58:   hotkeySs58,
		Netuid:       int64(netuid),
		StakeRao:     int64(self.sale.StakeRao),
		FreeTaoRao:   int64(self.sale.Balance.FreeRao),
		PriceRao:     int64(self.sale.PriceRao),
		MinAmountRao: int64(self.minAmountRao),
		MaxAmountRao: int64(self.maxAmountRao),
		FeeTaoRao:    int64(self.feeRao),
		FeeAlphaRao:  int64(self.feeAlphaRao),
		FeeInAlpha:   self.feeInAlpha,
	}
}

func bittensorConvertResultFromErr(coldkey [32]byte, err error) *BittensorConvertEstimateResult {
	coldkeySs58, _ := ss58.Encode(coldkey, SnSs58Prefix)
	return &BittensorConvertEstimateResult{ColdkeySs58: coldkeySs58, Error: bittensorConvertErrorFromErr(err)}
}

// bittensorConvertErrorFromErr says what went wrong reading or signing, in
// words a sheet can show. The detail stays out: it can name endpoints.
func bittensorConvertErrorFromErr(err error) *SnError {
	switch {
	case subtensor.IsRuntimeChanged(err):
		return newSnError(BittensorConvertErrorRuntimeChanged, "The Bittensor network has changed how this works. Update the app to convert.")
	case errors.Is(err, errBittensorChainWrongChain):
		return newSnError(BittensorChainErrorWrongChain, "The Bittensor network endpoint in this app's settings is for another chain.")
	case errors.Is(err, errBittensorConvertNoPrice):
		return newSnError(BittensorConvertErrorPriceMoved, "The subnet has no price to convert at right now. Try again later.")
	case errors.Is(err, errBittensorConvertPoolTooShallow):
		return newSnError(BittensorConvertErrorPriceImpact, "The subnet's pool is too small to convert this right now. Try again later.")
	}
	snErr := snErrorFromErr(err)
	switch snErr.Code {
	case SnErrorCodeChainRpcUnreachable:
		return newSnError(snErr.Code, "Couldn't reach the Bittensor network. Try again in a minute.")
	default:
		return newSnError(SnErrorCodeChainRpcError, "The Bittensor network couldn't answer. Try again in a minute.")
	}
}

// convertEstimate is the estimate for an amount.
func (self *bittensorChainEngine) convertEstimate(ctx context.Context, coldkey [32]byte, amountRao int64) *BittensorConvertEstimateResult {
	return self.convertEstimateWith(ctx, coldkey, amountRao, true)
}

func (self *bittensorChainEngine) convertEstimateWith(ctx context.Context, coldkey [32]byte, amountRao int64, cached bool) *BittensorConvertEstimateResult {
	state, err := self.convertState(ctx, coldkey, cached)
	if err != nil {
		return bittensorConvertResultFromErr(coldkey, err)
	}
	return self.convertEstimateFrom(ctx, state, amountRao)
}

func (self *bittensorChainEngine) convertEstimateFrom(ctx context.Context, state *bittensorConvertState, amountRao int64) *BittensorConvertEstimateResult {
	hotkeySs58, _ := ss58.Encode(state.hotkey, SnSs58Prefix)
	result := state.newResult(hotkeySs58, self.pins.netuid)
	result.AmountRao = amountRao
	if refusal := state.refuseStake(); refusal != nil {
		result.Error = refusal
		return result
	}
	if amountRao == 0 {
		return result
	}
	amount := uint64(max(amountRao, 0))
	if refusal := state.refuseAmount(amountRao); refusal != nil {
		result.Error = refusal
		return result
	}
	simulation, err := self.chain.SimulateSell(ctx, self.pins.netuid, amount)
	if err != nil {
		result.Error = bittensorConvertErrorFromErr(err)
		return result
	}
	// the runtime's simulation answers zeros when it cannot run the sale
	if simulation.TaoRao == 0 || simulation.AlphaRao+simulation.AlphaFeeRao != amount {
		result.Error = newSnError(BittensorConvertErrorPriceImpact, "The subnet's pool can't take this much at once. Try a smaller amount.")
		return result
	}
	minTao, limit, impactBps, err := bittensorConvertPromise(state.sale.PriceRao, simulation)
	result.PriceImpactBps = int64(impactBps)
	switch {
	case errors.Is(err, errBittensorConvertPoolTooShallow):
		result.Error = newSnError(BittensorConvertErrorPriceImpact, "Converting this much at once would move the price too far. Convert less at a time.")
		return result
	case err != nil || minTao == 0:
		result.Error = bittensorConvertErrorFromErr(errBittensorConvertNoPrice)
		return result
	}
	// the runtime refuses a sale under its floor, and keeps the fee, unless the
	// sale empties the position, which only a key paying in TAO can aim for
	emptiesPosition := !state.feeInAlpha && amount == state.sale.StakeRao
	if minTao < state.floorRao() && !emptiesPosition {
		result.Error = newSnError(BittensorConvertErrorAmountTooLow, fmt.Sprintf(
			"The network's smallest conversion is about %s.",
			FormatAlpha(int64(max(state.minAmountRao, amount+1))),
		))
		return result
	}
	expected := simulation.TaoRao
	// what the sale leaves: below the nominator minimum the runtime sells it too
	left := state.sale.StakeRao - amount
	if state.feeInAlpha {
		left -= min(left, state.feeAlphaRao)
	}
	minimumLeft, ok := bittensorMulDiv(state.sale.NominatorMinimumRao, uint64(SnRaoPerAlpha), state.sale.PriceRao)
	if !ok {
		result.Error = bittensorConvertErrorFromErr(errBittensorConvertNoPrice)
		return result
	}
	if 0 < left && left < minimumLeft {
		swept, err := self.chain.SimulateSell(ctx, self.pins.netuid, left)
		if err != nil {
			result.Error = bittensorConvertErrorFromErr(err)
			return result
		}
		result.RemainderSweptAlphaRao = int64(left)
		result.RemainderSweptTaoRao = int64(swept.TaoRao)
		expected += swept.TaoRao
	}
	result.ExpectedTaoRao = int64(expected)
	result.ExpectedNetTaoRao = result.ExpectedTaoRao
	if !state.feeInAlpha {
		result.ExpectedNetTaoRao -= int64(state.feeRao)
	}
	result.MinTaoRao = int64(minTao)
	result.LimitPriceRao = int64(limit)
	result.PoolFeeAlphaRao = int64(simulation.AlphaFeeRao)
	result.CanConvert = true
	result.Notice = state.notice(result)
	return result
}

// notice says what a sheet's numbers do not: where the fee comes from, and that
// the rest of the stake goes too.
func (self *bittensorConvertState) notice(result *BittensorConvertEstimateResult) string {
	notices := []string{}
	if result.FeeInAlpha {
		notices = append(notices, fmt.Sprintf(
			"This key has no TAO, so the network fee of about %s comes out of its %s.",
			FormatAlpha(result.FeeAlphaRao),
			SnAlphaSymbol,
		))
	}
	if 0 < result.RemainderSweptAlphaRao {
		notices = append(notices, fmt.Sprintf(
			"The other %s converts too, because the network doesn't keep stake worth less than %s.",
			FormatAlpha(result.RemainderSweptAlphaRao),
			FormatTao(int64(self.sale.NominatorMinimumRao)),
		))
	}
	return strings.Join(notices, " ")
}

// refuseStake says why this key can convert nothing, whatever the amount.
func (self *bittensorConvertState) refuseStake() *SnError {
	switch {
	case self.sale.StakeRao == 0:
		return newSnError(BittensorConvertErrorNothingToConvert, "There's no "+SnAlphaSymbol+" on this key to convert yet.")
	case self.maxAmountRao == 0:
		return newSnError(BittensorConvertErrorBalanceTooLow, fmt.Sprintf(
			"This key holds %s, which is too little to convert: the network needs more than that to cover its fee and its minimum sale.",
			FormatAlpha(int64(self.sale.StakeRao)),
		))
	}
	return nil
}

// refuseAmount says why this amount cannot be converted.
func (self *bittensorConvertState) refuseAmount(amountRao int64) *SnError {
	switch {
	case amountRao < int64(self.minAmountRao):
		return newSnError(BittensorConvertErrorAmountTooLow, fmt.Sprintf(
			"The network's smallest conversion is about %s.",
			FormatAlpha(int64(self.minAmountRao)),
		))
	case int64(self.maxAmountRao) < amountRao:
		if self.feeInAlpha {
			return newSnError(BittensorConvertErrorAmountTooHigh, fmt.Sprintf(
				"You can convert up to %s. This key has no TAO, so the rest pays the network fee.",
				FormatAlpha(int64(self.maxAmountRao)),
			))
		}
		return newSnError(BittensorConvertErrorAmountTooHigh, fmt.Sprintf(
			"You can convert up to %s.",
			FormatAlpha(int64(self.maxAmountRao)),
		))
	}
	return nil
}

// convertEstimateForBurn is the estimate for just enough to register a hotkey.
func (self *bittensorChainEngine) convertEstimateForBurn(ctx context.Context, coldkey [32]byte) *BittensorConvertEstimateResult {
	state, err := self.convertState(ctx, coldkey, true)
	if err != nil {
		return bittensorConvertResultFromErr(coldkey, err)
	}
	fail := func(err error) *BittensorConvertEstimateResult {
		result := state.newResult("", self.pins.netuid)
		result.Error = bittensorConvertErrorFromErr(err)
		return result
	}
	burn, err := self.burn(ctx, state.view)
	if err != nil {
		return fail(err)
	}
	// a real registration's bytes, priced and never signed: the hotkey does not change the fee
	register, err := subtensor.NewRegisterLimit(self.pins.netuid, state.hotkey, max(burn, 1))
	if err != nil {
		return fail(err)
	}
	registerFee, err := self.quoteFee(ctx, state.view, coldkey, register)
	if err != nil {
		return fail(err)
	}
	withMargin, ok := bittensorMulDivCeil(burn+registerFee, bittensorBasisPoints+bittensorConvertBurnMarginBps, bittensorBasisPoints)
	if !ok {
		return fail(errBittensorConvertNoPrice)
	}
	// the fee and the burn have to leave the key its deposit
	needed := withMargin + max(state.depositRao, state.sale.Balance.FrozenRao)
	finish := func(result *BittensorConvertEstimateResult) *BittensorConvertEstimateResult {
		result.BurnRao = int64(burn)
		result.RegisterFeeRao = int64(registerFee)
		result.BurnTaoNeededRao = int64(needed)
		return result
	}
	if needed <= state.sale.Balance.FreeRao {
		result := self.convertEstimateFrom(ctx, state, 0)
		result.Error = nil
		result.BurnAlreadyCovered = true
		return finish(result)
	}
	target := needed - state.sale.Balance.FreeRao
	if !state.feeInAlpha {
		target += state.feeRao
	}
	// the sale has to pay enough even at its limit, and at least the runtime's floor
	target = max(target, state.floorRao())
	amount, err := self.convertAmountPaying(ctx, target, state.sale.PriceRao)
	if err != nil {
		return fail(err)
	}
	amount = max(amount, state.minAmountRao)
	if state.maxAmountRao < amount {
		result := self.convertEstimateFrom(ctx, state, 0)
		if result.Error == nil {
			result.Error = newSnError(BittensorConvertErrorBalanceTooLow, fmt.Sprintf(
				"Registering takes about %s from this key, and it holds %s.",
				FormatAlpha(int64(amount+state.feeReserveRao)),
				FormatAlpha(int64(state.sale.StakeRao)),
			))
		}
		return finish(result)
	}
	return finish(self.convertEstimateFrom(ctx, state, int64(amount)))
}

// convertSubmit checks the estimate still holds, then signs and sends. A refusal
// here signs nothing.
func (self *bittensorChainEngine) convertSubmit(ctx context.Context, signer subtensor.Signer, estimate *BittensorConvertEstimateResult) *BittensorConvertSubmission {
	refuse := func(failure *SnError) *BittensorConvertSubmission {
		return &BittensorConvertSubmission{Error: failure}
	}
	if estimate == nil || !estimate.CanConvert || estimate.AmountRao <= 0 || estimate.LimitPriceRao <= 0 || estimate.MinTaoRao <= 0 {
		return refuse(newSnError(BittensorConvertErrorPriceMoved, "Estimate the amount again first."))
	}
	coldkey := signer.PublicKey()
	estimated, err := snColdkeyBytes(estimate.ColdkeySs58)
	if err != nil || estimated != coldkey {
		return refuse(newSnError(BittensorConvertErrorKeyMismatch, "This recovery phrase doesn't open the key this was estimated for."))
	}
	amount, limit, minTao := uint64(estimate.AmountRao), uint64(estimate.LimitPriceRao), uint64(estimate.MinTaoRao)
	fresh := self.convertEstimateWith(ctx, coldkey, estimate.AmountRao, false)
	if fresh.Error != nil {
		return refuse(fresh.Error)
	}
	priceMoved := newSnError(BittensorConvertErrorPriceMoved, "The price has moved since this estimate. Check the new estimate and try again.")
	price := uint64(fresh.PriceRao)
	// the chain would refuse it and keep the fee
	if price < limit {
		return refuse(priceMoved)
	}
	// no estimate of this sdk asks for a limit this far under the price
	floor, ok := bittensorMulDiv(price, bittensorBasisPoints-bittensorConvertLimitSanityBps, bittensorBasisPoints)
	if !ok || limit < floor {
		return refuse(priceMoved)
	}
	simulation, err := self.chain.SimulateSell(ctx, self.pins.netuid, amount)
	if err != nil {
		return refuse(bittensorConvertErrorFromErr(err))
	}
	if simulation.TaoRao < minTao {
		return refuse(priceMoved)
	}
	call, err := subtensor.NewRemoveStakeLimit(self.pins.escrowHotkey, self.pins.netuid, amount, limit)
	if err != nil {
		return refuse(bittensorConvertErrorFromErr(err))
	}
	signed, err := self.signCall(ctx, signer, call)
	if err != nil {
		return refuse(bittensorConvertErrorFromErr(err))
	}
	first, last := signed.landingBlocks()
	submission := &BittensorConvertSubmission{
		TxHash:      "0x" + hex.EncodeToString(signed.signed.Hash[:]),
		ColdkeySs58: estimate.ColdkeySs58,
		FirstBlock:  int64(first),
		LastBlock:   int64(last),
	}
	// whatever comes of it, what a sheet draws next reads the chain again
	defer forgetConvertState()
	if err := self.chain.Submit(ctx, signed.signed); err != nil {
		var refusal *subtensor.SubmitRefusal
		if errors.As(err, &refusal) {
			code, message := bittensorConvertPoolRefusal(refusal)
			return refuse(newSnError(code, message))
		}
		// the node may have taken it before the answer was lost
		code, message := bittensorConvertWaitFailure(err)
		submission.Error = newSnError(code, message)
	}
	return submission
}

// convertWait waits for a submission's finalized outcome. An endpoint that fails
// on the way is tried again until the context ends.
func (self *bittensorChainEngine) convertWait(ctx context.Context, submission *BittensorConvertSubmission) *BittensorConvertReceipt {
	fail := func(code string, message string) *BittensorConvertReceipt {
		return &BittensorConvertReceipt{Error: newSnError(code, message)}
	}
	if submission == nil || submission.TxHash == "" {
		return fail(BittensorConvertErrorRefused, "Nothing was sent to wait for.")
	}
	hash, hashErr := evm.ParseHexBytes(submission.TxHash)
	coldkey, coldkeyErr := snColdkeyBytes(submission.ColdkeySs58)
	first, last := uint64(max(submission.FirstBlock, 0)), uint64(max(submission.LastBlock, 0))
	if hashErr != nil || len(hash) != 32 || coldkeyErr != nil || first == 0 || last < first || subtensor.EraPeriodBlocks < last-first {
		return fail(BittensorConvertErrorRefused, "Nothing was sent to wait for.")
	}
	defer forgetConvertState()
	var meta *subtensor.Metadata
	for meta == nil {
		view, _, err := self.quoteView(ctx)
		switch {
		case err == nil:
			meta = view.meta
		case errors.Is(err, errBittensorChainWrongChain):
			failure := bittensorConvertErrorFromErr(err)
			return fail(failure.Code, failure.Message)
		default:
			if err := self.sleep(ctx, self.poll); err != nil {
				code, message := bittensorConvertWaitFailure(err)
				return fail(code, message)
			}
		}
	}
	outcome, err := self.waitCall(ctx, meta, [32]byte(hash), first, last)
	if err != nil {
		code, message := bittensorConvertWaitFailure(err)
		return fail(code, message)
	}
	receipt := bittensorConvertReceiptFrom(outcome, submission.TxHash, coldkey, self.pins.escrowHotkey)
	if !outcome.Success {
		code, message := bittensorConvertDispatchFailure(outcome.Failure)
		receipt.Error = newSnError(code, message)
	}
	return receipt
}

// bittensorConvertWaitFailure says what it means when a sent transaction's result
// cannot be told.
func bittensorConvertWaitFailure(err error) (string, string) {
	if errors.Is(err, subtensor.ErrExpired) {
		return BittensorConvertErrorExpired, "This conversion didn't land in time and now can't. Nothing was charged. You can try again."
	}
	return BittensorConvertErrorOutcomeUnknown, "This conversion may have been sent, and its result couldn't be read. Check your balances in a few minutes before trying again."
}

// bittensorConvertPoolRefusal reads the chain's refusal of a transaction it did
// not accept: 1010 for an invalid one, 1012 to 1014 for one like it in the pool.
func bittensorConvertPoolRefusal(refusal *subtensor.SubmitRefusal) (string, string) {
	const pending = "Another conversion from this key hasn't landed yet. Wait for it, then check your balances."
	switch refusal.Code {
	case 1012, 1013, 1014:
		return BittensorConvertErrorPending, pending
	}
	data := strings.ToLower(refusal.Data)
	switch {
	case strings.Contains(data, "outdated"), strings.Contains(data, "stale"):
		return BittensorConvertErrorPending, pending
	case strings.Contains(data, "pay some fees"):
		return BittensorConvertErrorBalanceTooLow, "This key holds too little to pay the network fee."
	case strings.Contains(data, "bad signature"):
		return BittensorConvertErrorRuntimeChanged, "The Bittensor network has changed how this works. Update the app to convert."
	}
	return BittensorConvertErrorRefused, "The Bittensor network refused this conversion. Nothing was charged."
}

// bittensorConvertDispatchFailure reads a failed sale: "Pallet.Error".
func bittensorConvertDispatchFailure(failure string) (string, string) {
	const feeKept = " The network fee was still charged."
	switch failure {
	case "SubtensorModule.SlippageTooHigh", "Swap.PriceLimitExceeded":
		return BittensorConvertErrorPriceMoved, "The price fell below the limit, so nothing was sold." + feeKept
	case "SubtensorModule.NotEnoughStakeToWithdraw":
		return BittensorConvertErrorAmountTooHigh, "This key held less than this amount by the time it ran, so nothing was sold." + feeKept
	case "SubtensorModule.AmountTooLow":
		return BittensorConvertErrorAmountTooLow, "This amount was below the network's smallest conversion by the time it ran, so nothing was sold." + feeKept
	}
	return BittensorConvertErrorFailed, "The Bittensor network couldn't complete this conversion, so nothing was sold." + feeKept
}

func bittensorConvertReceiptFrom(outcome *subtensor.Outcome, txHash string, coldkey [32]byte, hotkey [32]byte) *BittensorConvertReceipt {
	receipt := &BittensorConvertReceipt{
		TxHash:         txHash,
		BlockNumber:    int64(outcome.Block.Number),
		ExtrinsicIndex: int64(outcome.ExtrinsicIndex),
		FeeTaoRao:      int64(outcome.TaoFeeRao),
	}
	if outcome.AlphaFee != nil {
		receipt.FeeAlphaRao = int64(outcome.AlphaFee.AlphaRao)
		receipt.FeeTaoRao = int64(outcome.AlphaFee.TaoRao)
	}
	feeSaleFound := false
	for _, sale := range outcome.Sales {
		if sale.Coldkey != coldkey || sale.Hotkey != hotkey {
			continue
		}
		// the sale that paid the fee is first, at no pool fee, and the TAO it
		// raised over the final fee came back to the key
		if outcome.AlphaFee != nil && !feeSaleFound && sale.AlphaRao == outcome.AlphaFee.AlphaRao && sale.PoolFeeAlphaRao == 0 {
			feeSaleFound = true
			if outcome.AlphaFee.TaoRao < sale.TaoRao {
				receipt.TaoReceivedRao += int64(sale.TaoRao - outcome.AlphaFee.TaoRao)
			}
			continue
		}
		receipt.AlphaSoldRao += int64(sale.AlphaRao + sale.PoolFeeAlphaRao)
		receipt.TaoReceivedRao += int64(sale.TaoRao)
	}
	return receipt
}
