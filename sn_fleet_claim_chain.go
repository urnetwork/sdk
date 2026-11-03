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

	"github.com/ChainSafe/go-schnorrkel"

	"github.com/urnetwork/sdk/internal/subtensor"
)

// a sr25519 key the flow signs a transaction with, for as long as the transaction
type snFleetSigner struct {
	secret *schnorrkel.SecretKey
	public [32]byte
}

func newSnFleetSigner(mini *schnorrkel.MiniSecretKey) (*snFleetSigner, error) {
	secret := mini.ExpandEd25519()
	public, err := secret.Public()
	if err != nil {
		return nil, errors.New("this key can't be used")
	}
	return &snFleetSigner{secret: secret, public: public.Encode()}, nil
}

func (self *snFleetSigner) PublicKey() [32]byte {
	return self.public
}

func (self *snFleetSigner) Sign(payload []byte) ([64]byte, error) {
	signature, err := self.secret.Sign(schnorrkel.NewSigningContext([]byte(bittensorLocalKeySigningContext), payload))
	if err != nil {
		return [64]byte{}, errBittensorLocalKeySign
	}
	return signature.Encode(), nil
}

func snFleetSignerFromPhrase(phrase string) (*snFleetSigner, error) {
	mini, err := bittensorLocalKeyMiniSecret(phrase)
	if err != nil {
		return nil, err
	}
	return newSnFleetSigner(mini)
}

func snFleetSignerFromSeed(seed [32]byte) (*snFleetSigner, error) {
	mini, err := schnorrkel.NewMiniSecretKeyFromRaw(seed)
	if err != nil {
		return nil, errors.New("the fleet hotkey seed can't be used")
	}
	return newSnFleetSigner(mini)
}

// snFleetClaimEngineChain builds its engine when the flow first needs the chain, so
// a claim with nothing to do reads and dials nothing.
type snFleetClaimEngineChain struct {
	settings *SnChainSettings
}

func newSnFleetClaimEngineChain(settings *SnChainSettings) *snFleetClaimEngineChain {
	return &snFleetClaimEngineChain{settings: settings}
}

func (self *snFleetClaimEngineChain) engine() (*bittensorChainEngine, error) {
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

// A sale's callback, turned back into one answer for the flow's pass.
type snFleetConvertResult struct {
	err error
}

func (self *snFleetConvertResult) Sent(string) {}

func (self *snFleetConvertResult) Confirmed(*BittensorConvertReceipt) {}

func (self *snFleetConvertResult) Failed(code string, message string) {
	self.err = fmt.Errorf("%s: %s", code, message)
}

func (self *snFleetClaimEngineChain) ConvertForBurn(ctx context.Context, coldkeyPhrase string, plan *snFleetBurnPlan) error {
	if plan == nil || plan.Convert == nil {
		return errors.New("there is no sale to make")
	}
	engine, err := self.engine()
	if err != nil {
		return err
	}
	signer, err := snFleetSignerFromPhrase(coldkeyPhrase)
	if err != nil {
		return err
	}
	result := &snFleetConvertResult{}
	engine.convertSubmit(ctx, signer, plan.Convert, result)
	return result.err
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

func (self *snFleetClaimEngineChain) RegisterLimit(ctx context.Context, coldkeyPhrase string, netuid uint16, hotkey [32]byte, limitRao uint64) error {
	engine, err := self.engine()
	if err != nil {
		return err
	}
	signer, err := snFleetSignerFromPhrase(coldkeyPhrase)
	if err != nil {
		return err
	}
	call, err := subtensor.NewRegisterLimit(netuid, hotkey, limitRao)
	if err != nil {
		return err
	}
	return snFleetClaimOutcome(engine.submitCall(ctx, signer, call, nil))
}

func (self *snFleetClaimEngineChain) SetFleetCommitment(ctx context.Context, hotkeySeed [32]byte, netuid uint16, hash [32]byte) error {
	engine, err := self.engine()
	if err != nil {
		return err
	}
	signer, err := snFleetSignerFromSeed(hotkeySeed)
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
