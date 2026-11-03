//go:build !ios_extension && !js

package sdk

// The chain side of SnFleetClaim, on the sdk's one Substrate encoder
// (internal/subtensor, bittensor_chain.go). Every write here builds one fixed call
// from typed values. Nothing here can sign a call or an amount a server chose: the
// chain's own answers, read here, set the burn, and the pins in the engine set the
// chain and the subnet.

import (
	"context"
	"errors"
	"fmt"

	"github.com/urnetwork/sdk/internal/subtensor"
)

// snFleetClaimEngineChain builds its engine when the flow first needs the chain, so
// a claim with nothing to do reads and dials nothing.
type snFleetClaimEngineChain struct {
	settings *SnChainSettings
	// the engine, in place of one from the settings
	testEngine *bittensorChainEngine
}

func newSnFleetClaimEngineChain(settings *SnChainSettings) *snFleetClaimEngineChain {
	return &snFleetClaimEngineChain{settings: settings}
}

func (self *snFleetClaimEngineChain) engine() (*bittensorChainEngine, error) {
	if self.testEngine != nil {
		return self.testEngine, nil
	}
	engine, failure := newBittensorChainEngine(self.settings)
	if failure != nil {
		return nil, errors.New(failure.Message)
	}
	return engine, nil
}

func (self *snFleetClaimEngineChain) Block(ctx context.Context) (uint64, error) {
	engine, err := self.engine()
	if err != nil {
		return 0, err
	}
	head, err := engine.chain.FinalizedHead(ctx)
	if err != nil {
		return 0, err
	}
	return head.Number, nil
}

func (self *snFleetClaimEngineChain) HotkeyUid(ctx context.Context, netuid uint16, hotkey [32]byte) (int64, bool, error) {
	engine, err := self.engine()
	if err != nil {
		return 0, false, err
	}
	uid, registered, err := engine.chain.ReadHotkeyUid(ctx, netuid, hotkey)
	return int64(uid), registered, err
}

// The codes that mean this key can't make the sale, not that something failed: the
// flow goes on to the top-up.
func snFleetClaimCannotConvert(code string) bool {
	switch code {
	case BittensorConvertErrorNothingToConvert, BittensorConvertErrorBalanceTooLow,
		BittensorConvertErrorAmountTooLow, BittensorConvertErrorAmountTooHigh,
		BittensorConvertErrorPriceImpact:
		return true
	}
	return false
}

func (self *snFleetClaimEngineChain) BurnPlan(ctx context.Context, coldkey [32]byte) (*snFleetBurnPlan, error) {
	engine, err := self.engine()
	if err != nil {
		return nil, err
	}
	estimate := engine.convertEstimateForBurn(ctx, coldkey)
	if estimate.Error != nil && !snFleetClaimCannotConvert(estimate.Error.Code) {
		return nil, errors.New(estimate.Error.Message)
	}
	if estimate.BurnRao <= 0 && estimate.RegisterFeeRao <= 0 {
		// a key with no alpha at all stops before the burn is read
		return self.burnPlanWithoutSale(ctx, engine, coldkey)
	}
	plan := &snFleetBurnPlan{BurnRao: uint64(estimate.BurnRao), FeeRao: uint64(estimate.RegisterFeeRao)}
	// exactly what registering takes now, with the key's deposit left over. The sale's
	// own margin is for a burn moving during the sale; a registration happens now.
	deposit, err := self.deposit(ctx, engine)
	if err != nil {
		return nil, err
	}
	plan.Covered = plan.BurnRao+plan.FeeRao+deposit <= uint64(estimate.FreeTaoRao)
	if !plan.Covered && estimate.CanConvert && !estimate.BurnAlreadyCovered {
		plan.Convert = estimate
	}
	return plan, nil
}

func (self *snFleetClaimEngineChain) deposit(ctx context.Context, engine *bittensorChainEngine) (uint64, error) {
	view, _, err := engine.quoteView(ctx)
	if err != nil {
		return 0, err
	}
	return view.meta.ExistentialDeposit()
}

func (self *snFleetClaimEngineChain) burnPlanWithoutSale(ctx context.Context, engine *bittensorChainEngine, coldkey [32]byte) (*snFleetBurnPlan, error) {
	view, hotkey, err := engine.quoteView(ctx)
	if err != nil {
		return nil, err
	}
	burn, err := engine.burn(ctx, view)
	if err != nil {
		return nil, err
	}
	register, err := subtensor.NewRegisterLimit(engine.pins.netuid, hotkey, max(burn, 1))
	if err != nil {
		return nil, err
	}
	fee, err := engine.quoteFee(ctx, view, coldkey, register)
	if err != nil {
		return nil, err
	}
	balance, err := engine.freeBalance(ctx, view, coldkey)
	if err != nil {
		return nil, err
	}
	deposit, err := view.meta.ExistentialDeposit()
	if err != nil {
		return nil, err
	}
	return &snFleetBurnPlan{BurnRao: burn, FeeRao: fee, Covered: burn+fee+deposit <= balance.FreeRao}, nil
}

func (self *snFleetClaimEngineChain) FleetCommitment(ctx context.Context, netuid uint16, hotkey [32]byte) ([32]byte, bool, error) {
	engine, err := self.engine()
	if err != nil {
		return [32]byte{}, false, err
	}
	return engine.chain.ReadFleetCommitment(ctx, netuid, hotkey)
}

// The sale the plan found, through the convert's own checks, which refuse before
// signing what the chain would refuse after taking the fee. It waits for the
// finalized outcome, so the next pass reads the balance the sale left.
func (self *snFleetClaimEngineChain) ConvertForBurn(ctx context.Context, coldkeyPhrase string, plan *snFleetBurnPlan) error {
	if plan == nil || plan.Convert == nil {
		return errors.New("there is no sale to make")
	}
	engine, err := self.engine()
	if err != nil {
		return err
	}
	signer, err := bittensorSignerFromPhrase(coldkeyPhrase)
	if err != nil {
		return err
	}
	submission := engine.convertSubmit(ctx, signer, plan.Convert)
	if submission.Error != nil && submission.TxHash == "" {
		return fmt.Errorf("%s: %s", submission.Error.Code, submission.Error.Message)
	}
	receipt := engine.convertWait(ctx, submission)
	if receipt.Error != nil {
		return fmt.Errorf("%s: %s", receipt.Error.Code, receipt.Error.Message)
	}
	return nil
}

func snFleetClaimOutcome(outcome *subtensor.Outcome, err error) error {
	if err != nil {
		return err
	}
	if outcome == nil || !outcome.Success {
		failure := ""
		if outcome != nil {
			failure = outcome.Failure
		}
		return fmt.Errorf("the transaction landed and failed: %s", failure)
	}
	return nil
}

// What register_limit would refuse, read before it's signed: the chain takes the fee
// before the call runs and keeps it when the call fails.
var (
	errSnFleetRegisterBurnRose      = errors.New("the burn rose past its limit")
	errSnFleetRegisterShort         = errors.New("the key can't pay the burn and its fee and keep its deposit")
	errSnFleetRegistrationClosed    = errors.New("the subnet isn't taking registrations")
	errSnFleetRegisterRegistered    = errors.New("the hotkey is registered already")
	errSnFleetHotkeyOwnedElsewhere  = errors.New("the hotkey belongs to another coldkey")
	errSnFleetRegisterCheckedFailed = errors.New("the registration can't be checked right now")
)

// register_limit, once everything it checks that can be read says it would pass: the
// burn within the limit, the key paying the burn and the fee and keeping its deposit,
// the subnet taking registrations, and the hotkey free and not another coldkey's.
func (self *snFleetClaimEngineChain) RegisterLimit(ctx context.Context, coldkeyPhrase string, netuid uint16, hotkey [32]byte, limitRao uint64) error {
	engine, err := self.engine()
	if err != nil {
		return err
	}
	signer, err := bittensorSignerFromPhrase(coldkeyPhrase)
	if err != nil {
		return err
	}
	coldkey := signer.PublicKey()
	call, err := subtensor.NewRegisterLimit(netuid, hotkey, limitRao)
	if err != nil {
		return err
	}
	if err := self.registerWouldPass(ctx, engine, coldkey, netuid, hotkey, limitRao, call); err != nil {
		return err
	}
	return snFleetClaimOutcome(engine.submitCall(ctx, signer, call, nil))
}

func (self *snFleetClaimEngineChain) registerWouldPass(ctx context.Context, engine *bittensorChainEngine, coldkey [32]byte, netuid uint16, hotkey [32]byte, limitRao uint64, call *subtensor.Call) error {
	unread := func(err error) error {
		return fmt.Errorf("%w: %w", errSnFleetRegisterCheckedFailed, err)
	}
	view, _, err := engine.quoteView(ctx)
	if err != nil {
		return unread(err)
	}
	burn, err := engine.chain.ReadBurn(ctx, view.meta, netuid)
	if err != nil {
		return unread(err)
	}
	if limitRao < burn {
		return errSnFleetRegisterBurnRose
	}
	fee, err := engine.quoteFee(ctx, view, coldkey, call)
	if err != nil {
		return unread(err)
	}
	balance, err := engine.freeBalance(ctx, view, coldkey)
	if err != nil {
		return unread(err)
	}
	deposit, err := view.meta.ExistentialDeposit()
	if err != nil {
		return unread(err)
	}
	// the fee comes off first, and what stays must cover the account's deposit or
	// what's frozen, whichever is more
	kept := max(deposit, balance.FrozenRao)
	if balance.FreeRao < fee || balance.FreeRao-fee < kept || balance.FreeRao-fee-kept < burn {
		return errSnFleetRegisterShort
	}
	allowed, err := engine.chain.ReadRegistrationAllowed(ctx, netuid)
	if err != nil {
		return unread(err)
	}
	if !allowed {
		return errSnFleetRegistrationClosed
	}
	if _, registered, err := engine.chain.ReadHotkeyUid(ctx, netuid, hotkey); err != nil {
		return unread(err)
	} else if registered {
		return errSnFleetRegisterRegistered
	}
	owner, owned, err := engine.chain.ReadHotkeyOwner(ctx, hotkey)
	if err != nil {
		return unread(err)
	}
	if owned && owner != coldkey {
		return errSnFleetHotkeyOwnedElsewhere
	}
	return nil
}

func (self *snFleetClaimEngineChain) SetFleetCommitment(ctx context.Context, hotkeySeed [32]byte, netuid uint16, hash [32]byte) error {
	engine, err := self.engine()
	if err != nil {
		return err
	}
	signer, err := bittensorSignerFromSeed(hotkeySeed)
	if err != nil {
		return err
	}
	call, err := subtensor.NewSetFleetCommitment(netuid, hash)
	if err != nil {
		return err
	}
	return snFleetClaimOutcome(engine.submitCall(ctx, signer, call, nil))
}

// A recovery phrase's two operations, from the local key.
type snFleetClaimLocalColdkey struct{}

func (snFleetClaimLocalColdkey) Address(phrase string) (string, error) {
	return BittensorLocalKeyAddress(phrase)
}

func (snFleetClaimLocalColdkey) SignMessage(phrase string, message string) (string, error) {
	return BittensorLocalKeySign(phrase, message)
}
