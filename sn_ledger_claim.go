package sdk

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"

	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk/sn/evm"
	"github.com/urnetwork/sdk/sn/merkle"
)

// Settlement v2 claims (APPS-SPEC.md, VERSION2-FINAL.md §7.1 claim). The
// coldkey's accounts come from GET /sn/ledger/account, which is untrusted: an
// account is claimed only in the pinned Vault2Address, only when its proof
// verifies here against vault2's roots(noId, rootIndex), and only for what
// vault2's claimed(noId, index) leaves of its cumulative. Each claim passes an
// eth_call preflight from the gas key, takes its gas limit from
// eth_estimateGas and at least snLedgerClaimGasFloor, needs the gas key to
// hold the limit × the price, and is signed with the gas key and sent to the
// chain endpoint. vault2 pays the leaf's coldkey whoever sends the claim, and
// its Claimed event says how much.
//
// A coldkey has an account in each of an operator's pools, its pool and its
// capture the flag pool (sn CAPTURETHEFLAG.md §10). One claim claims them all.
// Two or more accounts whose preflight passes are claimed in one transaction
// through the release claim batcher, STClaimBatcher (sn VERSION2-FINAL.md
// §10.2), which calls vault2's claim for each with a gas cap and reports each
// outcome; vault2 still pays each leaf's coldkey and emits its Claimed event.
// A single account is claimed alone, as are all of them without a batcher whose
// code is the release batcher's. An account is never in two transactions of one
// claim, unless a mined batch provably did not attempt it.
//
// A payer that can pay for one claim but not the batch (about 3.5M gas for two
// accounts against 2.6M for one) claims the largest account alone, and the
// others stay claimable (the owner, 2026-11-11): the device's gas key claims the
// accounts alone in turn, the largest first, and reports each it cannot pay for
// as needs_gas; an external wallet is given the largest account's claim alone,
// with the others named still claimable (SnLedgerClaimTransactionsForPayer).

// SnLedgerClaimCallback receives the progress of SnLedgerClaim. Failed
// messages start with an SnErrorCode* value followed by ": detail".
// SnErrorCodeAlreadyClaimed means another claim paid the account first,
// which is success for the account: the apps refresh. A failure that is not
// about one account (no wallet, v2 claims not configured, the ledger or the
// chain unreachable, nothing to claim) comes with noId and index 0. A pool
// whose ledger could not be read comes with its noId, index 0 and
// SnErrorCodeLedgerUnavailable: its account is unknown, and the other pools are
// claimed. The accounts of a batch are each Sent with the batch's txHash.
// SnErrorCodeNeedsGas for an account means the gas key could not pay for its
// claim and nothing was sent for it: it is still claimable.
type SnLedgerClaimCallback interface {
	Sent(noId int64, index int64, txHash string)
	Confirmed(noId int64, index int64, txHash string, amountRao int64)
	Failed(noId int64, index int64, message string)
	Done()
}

// SnLedgerUnsignedTx is a v2 claim transaction for an external signer: one
// account's claim to vault2, or, with Batch, one claim of several accounts
// through the claim batcher. Items lists the accounts it claims.
type SnLedgerUnsignedTx struct {
	// the account's; 0 for a batch, whose accounts are its Items
	NoId      int64 `json:"no_id"`
	Index     int64 `json:"index"`
	RootIndex int64 `json:"root_index"`
	ChainId   int64 `json:"chain_id"`
	// Vault2Address, or ClaimBatcherAddress for a batch, lowercase 0x
	To string `json:"to"`
	// 0x calldata of claim(noId, rootIndex, (index, coldkey, cumulative),
	// proof), or of claimMany(vault2, claims, gasPerClaim) for a batch
	Data string `json:"data"`
	// decimal wei, always "0"
	Value string `json:"value"`
	// what the claim pays now: the cumulatives less vault2's claimed
	AmountRao int64 `json:"amount_rao"`
	// the gas limit to sign with. A claim alone takes snLedgerClaimGasFloor
	// (2,600,000): a signer's own estimate follows vault2's staking-hotkey
	// list, which third parties can lengthen before the claim is included. A
	// batch takes the least limit at which the batcher attempts every item,
	// with a fifth more: a signer's estimate finds a lower limit, at which
	// the batcher skips items
	Gas int64 `json:"gas"`
	// Vault2Address, whose Claimed events say what each account was paid
	Vault string `json:"vault"`
	// To is the claim batcher
	Batch bool `json:"batch"`
	// the accounts the transaction claims, in order
	Items *SnLedgerClaimItemList `json:"items"`
}

// SnLedgerClaimItem is one account an SnLedgerUnsignedTx claims.
type SnLedgerClaimItem struct {
	NoId      int64 `json:"no_id"`
	Index     int64 `json:"index"`
	RootIndex int64 `json:"root_index"`
	// SnLedgerPoolKind*
	Kind  string `json:"kind"`
	Label string `json:"label"`
	// what the claim pays the account now
	AmountRao int64 `json:"amount_rao"`
}

type SnLedgerClaimItemList struct {
	exportedList[*SnLedgerClaimItem]
}

func NewSnLedgerClaimItemList() *SnLedgerClaimItemList {
	return &SnLedgerClaimItemList{
		exportedList: *newExportedList[*SnLedgerClaimItem](),
	}
}

type SnLedgerUnsignedTxList struct {
	exportedList[*SnLedgerUnsignedTx]
}

func NewSnLedgerUnsignedTxList() *SnLedgerUnsignedTxList {
	return &SnLedgerUnsignedTxList{
		exportedList: *newExportedList[*SnLedgerUnsignedTx](),
	}
}

const snLedgerNothingToClaim = SnErrorCodeNotClaimable + ": no settlement v2 account has anything to claim"

// snLedgerClaimGasFloor is the least gas limit a v2 claim is signed with: the
// plan's size for a claim (VERSION2-FINAL.md §10.2), which sn names
// onchain.ClaimBatchGasPerClaim (sn miner/onchain/v2_batch.go). A claim uses
// about 187,000 gas, but the runtime admits vault2's staking-precompile
// transfer only if the gas forwarded covers the transfer's declared weight
// (§7.3). Through runtime 477 that weight walked a full 256-entry
// StakingHotkeys list: a claim needed a limit of up to 2,443,953, and
// eth_estimateGas gave up to 2,574,029 (R-6, R-7). Runtime 478 walks only
// vault2's own list, at 8,750 gas an entry: with an empty list a claim needs
// 192,318 and is estimated at about 207,000, and with the 128 entries third
// parties can add, one transfer each, about 1.31M. They can add them between
// a claim's estimate and its inclusion, so a claim sized by its estimate
// alone runs out of gas. Only the gas used is charged, but the gas key must
// hold the limit × the price.
const snLedgerClaimGasFloor uint64 = 2_600_000

// The batch limits of sn's claim tools (miner/onchain/v2_batch.go): at most
// snLedgerBatchMaxClaims accounts in a batch, and at most the gas a
// transaction may name (R-7: the normal dispatch class's extrinsic weight
// limit, 13/15 of the block's gas limit less 1,500, and at most 64,998,500),
// less snLedgerBatchGasMargin. A batch's limit is sized to within
// snLedgerBatchTolerance of the least at which the batcher attempts every
// item.
const (
	snLedgerBatchMaxClaims        = 32
	snLedgerBatchMaxGas    uint64 = 64_998_500
	snLedgerBatchGasMargin uint64 = 10_000
	snLedgerBatchTolerance uint64 = 10_000
)

// snLedgerBatchCeiling is the most gas a batch names when the latest block
// reports blockGasLimit.
func snLedgerBatchCeiling(blockGasLimit uint64) uint64 {
	normal := blockGasLimit/15*13 + blockGasLimit%15*13/15
	takes := min(snLedgerBatchMaxGas, normal-min(normal, 1_500))
	return takes - min(takes, snLedgerBatchGasMargin)
}

// The node's message for a refused precompile call:
// evm error: Other("dispatch execution failed: <Error>").
var snRuntimeRefusalRe = regexp.MustCompile(`dispatch execution failed: (\w+)`)

// validateLedgerClaims reports whether v2 claims can be sent: rpc, chain id
// and Vault2Address are all set.
func (self *SnChainSettings) validateLedgerClaims() error {
	switch {
	case self == nil:
		return errors.New("nil chain settings")
	case len(self.rpcUrls) == 0:
		return errors.New("no rpc urls")
	case self.ChainId <= 0:
		return errors.New("chain id not set")
	}
	if _, err := self.vault2(); err != nil {
		return fmt.Errorf("vault2 address: %w", err)
	}
	return nil
}

type snLedgerClaimEngine struct {
	settings *SnChainSettings
	client   *evm.Client
	chainId  *big.Int
	vault2   [20]byte
	// the claim batcher; without one every account is claimed alone
	batcher     [20]byte
	hasBatcher  bool
	coldkey     [32]byte
	coldkeySs58 string
	baseUrl     string
	fetch       snArtifactFetcher
}

// snLedgerClaimPlan is an account a claim pays now: finalized in vault2, its
// proof verified against vault2's root, and amount its cumulative less
// vault2's claimed.
type snLedgerClaimPlan struct {
	noId      int64
	index     int64
	rootIndex int64
	kind      string
	label     string
	// the verified cumulative and proof, which a batch item claims with
	cumulative *big.Int
	proof      []merkle.LedgerNode
	// the claim calldata
	data   []byte
	amount int64
}

// item is the plan as a claim batcher item.
func (self *snLedgerClaimPlan) item(coldkey [32]byte) *evm.LedgerClaim {
	return &evm.LedgerClaim{
		NoId:       uint64(self.noId),
		RootIndex:  uint64(self.rootIndex),
		Index:      uint64(self.index),
		Coldkey:    coldkey,
		Cumulative: self.cumulative,
		Proof:      self.proof,
	}
}

// newSnLedgerClaimEngine reads the ledger from baseUrl, else the settings'
// artifact host, else the default host.
func newSnLedgerClaimEngine(settings *SnChainSettings, coldkeySs58 string, baseUrl string, fetch snArtifactFetcher) (*snLedgerClaimEngine, *SnError) {
	if err := settings.validateLedgerClaims(); err != nil {
		return nil, newSnError(SnErrorCodeChainNotConfigured, err.Error())
	}
	coldkeySs58 = strings.TrimSpace(coldkeySs58)
	coldkey, err := snColdkeyBytes(coldkeySs58)
	if err != nil {
		return nil, newSnError(SnErrorCodeInvalidAddress, err.Error())
	}
	vault2, _ := settings.vault2()
	// a batcher address that does not parse batches nothing
	batcher, hasBatcher, _ := settings.claimBatcher()
	if baseUrl == "" {
		baseUrl = settings.ArtifactBaseUrl
	}
	return &snLedgerClaimEngine{
		settings:    settings,
		client:      settings.chainClient(),
		chainId:     big.NewInt(settings.ChainId),
		vault2:      vault2,
		batcher:     batcher,
		hasBatcher:  hasBatcher,
		coldkey:     coldkey,
		coldkeySs58: coldkeySs58,
		baseUrl:     baseUrl,
		fetch:       fetch,
	}, nil
}

func (self *snLedgerClaimEngine) vault2Hex() string {
	return "0x" + hex.EncodeToString(self.vault2[:])
}

// plans reads the coldkey's accounts and returns, in the ledger's order,
// those a claim pays now (rule 1). Each account refused on the way is
// reported to fail, and so is each pool whose ledger could not be read, with
// index 0. An account only in the pending root is skipped, and so is one with
// nothing claimable in vault2 unless the ledger showed it with something
// claimable: then another claim paid it first.
func (self *snLedgerClaimEngine) plans(ctx context.Context, fail func(noId int64, index int64, message string)) ([]*snLedgerClaimPlan, *SnError) {
	read, snErr := snReadLedger(ctx, self.fetch, self.baseUrl, self.coldkeySs58)
	if snErr != nil {
		return nil, snErr
	}
	for _, unavailable := range read.unavailable {
		fail(unavailable.NoId, 0, fmt.Sprintf("%s: the ledger of %s (pool %d) could not be read", SnErrorCodeLedgerUnavailable, unavailable.Label, unavailable.NoId))
	}
	var finalized []*snLedgerEntry
	var datas [][]byte
	for _, entry := range read.entries {
		account := entry.account
		switch {
		case entry.rootIndex < 0:
		case account.Vault != self.vault2Hex():
			fail(account.NoId, account.Index, fmt.Sprintf("%s: the ledger lists this account in %s, not in the configured vault2 %s", SnErrorCodeNotClaimable, account.Vault, self.vault2Hex()))
		default:
			finalized = append(finalized, entry)
			datas = append(datas,
				evm.PackLedgerRoots(uint64(account.NoId), uint64(entry.rootIndex)),
				evm.PackLedgerClaimed(uint64(account.NoId), uint64(account.Index)),
			)
		}
	}
	if len(finalized) == 0 {
		return nil, nil
	}
	rets, errs, err := self.client.EthCallBatch(ctx, self.vault2, datas)
	if err != nil {
		return nil, snErrorFromErr(err)
	}
	plans := []*snLedgerClaimPlan{}
	for i, entry := range finalized {
		account := entry.account
		rootErr, claimedErr := errs[2*i], errs[2*i+1]
		var root *evm.LedgerRoot
		if rootErr == nil {
			root, rootErr = evm.DecodeLedgerRoot(rets[2*i])
		}
		var claimed *big.Int
		if claimedErr == nil {
			claimed, claimedErr = evm.DecodeUint(rets[2*i+1])
		}
		var rpcErr *evm.RpcError
		switch {
		case errors.As(rootErr, &rpcErr) && len(rpcErr.RevertData()) > 0:
			// roots(noId, rootIndex) reverts past the pool's last root
			fail(account.NoId, account.Index, fmt.Sprintf("%s: vault2 has no root %d in pool %d", SnErrorCodeProofMismatch, entry.rootIndex, account.NoId))
			continue
		case rootErr != nil || claimedErr != nil:
			fail(account.NoId, account.Index, SnErrorCodeChainRpcError+": "+errors.Join(rootErr, claimedErr).Error())
			continue
		}
		cumulative := big.NewInt(account.FinalizedRao)
		data, err := evm.PackLedgerClaim(uint64(account.NoId), uint64(entry.rootIndex), uint64(account.Index), self.coldkey, cumulative, entry.proof)
		if err != nil || !merkle.VerifyLedgerProof(root.Root, root.Total, root.LeafCount, uint64(account.Index), self.coldkey, cumulative, entry.proof) {
			fail(account.NoId, account.Index, SnErrorCodeProofMismatch+": the ledger's proof does not verify against vault2's root")
			continue
		}
		if claimed.Cmp(cumulative) >= 0 {
			if account.ClaimableRao > 0 {
				fail(account.NoId, account.Index, SnErrorCodeAlreadyClaimed+": vault2 has paid this account its finalized cumulative")
			}
			continue
		}
		plans = append(plans, &snLedgerClaimPlan{
			noId:       account.NoId,
			index:      account.Index,
			rootIndex:  entry.rootIndex,
			kind:       account.Kind,
			label:      account.Label,
			cumulative: cumulative,
			proof:      entry.proof,
			data:       data,
			amount:     account.FinalizedRao - claimed.Int64(),
		})
	}
	return plans, nil
}

// refusal names why vault2 refuses the plan's claim (rule 4). NothingToClaim
// means another claim paid the account first: already_claimed, success for
// the account. InvalidProof and UnknownRoot are proof_mismatch. A revert
// without data is the staking precompile refusing the transfer, and the
// runtime's reason is read by simulating the transfer: its transfer floor
// (AmountTooLow) is not_claimable, any other refusal claim_failed. A refused
// claim changes nothing, so the account stays claimable.
func (self *snLedgerClaimEngine) refusal(ctx context.Context, plan *snLedgerClaimPlan, err error) string {
	var rpcErr *evm.RpcError
	if !errors.As(err, &rpcErr) {
		return snFailMessage(err)
	}
	if len(rpcErr.RevertData()) == 0 && (rpcErr.Code == 3 || strings.Contains(strings.ToLower(rpcErr.Message), "revert")) {
		return self.runtimeRefusalMessage(ctx, plan)
	}
	return snLedgerVaultRefusal(rpcErr.Reason())
}

// snLedgerVaultRefusal is the message of vault2's refusal named reason.
func snLedgerVaultRefusal(reason string) string {
	switch reason {
	case "NothingToClaim":
		return SnErrorCodeAlreadyClaimed + ": NothingToClaim: another claim paid this account first"
	case "InvalidProof", "UnknownRoot":
		return SnErrorCodeProofMismatch + ": " + reason
	default:
		return SnErrorCodeClaimFailed + ": " + reason
	}
}

// runtimeRefusalMessage is the message of the staking precompile refusing
// the plan's transfer: its transfer floor is not_claimable, any other refusal
// claim_failed.
func (self *snLedgerClaimEngine) runtimeRefusalMessage(ctx context.Context, plan *snLedgerClaimPlan) string {
	reason := self.runtimeRefusal(ctx, plan)
	if reason == "AmountTooLow" {
		return SnErrorCodeNotClaimable + ": AmountTooLow: the claim is below the runtime's transfer floor"
	}
	return SnErrorCodeClaimFailed + ": " + reason
}

// batchRefusal names why the batcher's call of the plan's claim reverted,
// from the first four bytes of its revert data: vault2's refusals as a claim
// alone reads them, and a zero selector, a call that ran out of its gas cap
// as one the runtime refuses does, by simulating the transfer.
func (self *snLedgerClaimEngine) batchRefusal(ctx context.Context, plan *snLedgerClaimPlan, selector [4]byte) string {
	if selector == ([4]byte{}) {
		return self.runtimeRefusalMessage(ctx, plan)
	}
	return snLedgerVaultRefusal(evm.DecodeRevert(selector[:]))
}

// runtimeRefusal simulates vault2's transfer of the plan's amount to the
// coldkey on the escrow hotkey. eth_call from vault2's address runs the
// staking precompile as vault2's coldkey, and the node's message names the
// runtime's reason (scripts/runtime-rehearsal/README.md, R-3).
func (self *snLedgerClaimEngine) runtimeRefusal(ctx context.Context, plan *snLedgerClaimPlan) string {
	reason := "the claim reverted without a reason"
	escrow, err := self.client.EthCall(ctx, self.vault2, evm.PackEscrowHotkey())
	if err != nil || len(escrow) != 32 {
		return reason
	}
	data := evm.PackTransferStake(self.coldkey, [32]byte(escrow), uint64(self.settings.Netuid), uint64(plan.amount))
	_, err = self.client.EthCallFrom(ctx, self.vault2, evm.StakingPrecompile, data)
	var rpcErr *evm.RpcError
	if errors.As(err, &rpcErr) {
		reason = rpcErr.Message
		if match := snRuntimeRefusalRe.FindStringSubmatch(reason); match != nil {
			reason = match[1]
		}
	}
	return reason
}

// claimedAmount is the amount of vault2's Claimed event for the plan's
// account in the receipt (rule 5).
func (self *snLedgerClaimEngine) claimedAmount(receipt *evm.Receipt, plan *snLedgerClaimPlan) (int64, bool) {
	for _, log := range receipt.Logs {
		event, ok := evm.DecodeLedgerClaimed(log)
		if ok && log.Address == self.vault2 && event.NoId.Cmp(big.NewInt(plan.noId)) == 0 &&
			event.Index.Cmp(big.NewInt(plan.index)) == 0 && event.Coldkey == self.coldkey && event.Amount.IsInt64() {
			return event.Amount.Int64(), true
		}
	}
	return 0, false
}

// send claims one account with the gas key (rule 3) and reports through the
// callback.
func (self *snLedgerClaimEngine) send(ctx context.Context, key *secp.PrivateKey, plan *snLedgerClaimPlan, callback SnLedgerClaimCallback) {
	failed := func(message string) {
		callback.Failed(plan.noId, plan.index, message)
	}
	from := evm.AddressOf(key)
	if _, err := self.client.EthCallFrom(ctx, from, self.vault2, plan.data); err != nil {
		failed(self.refusal(ctx, plan, err))
		return
	}
	gas, err := self.client.EstimateGas(ctx, from, self.vault2, plan.data, nil)
	if err != nil {
		failed(self.refusal(ctx, plan, err))
		return
	}
	// the estimate plus a quarter, and never less than a claim needed before
	// runtime 478
	gas = max(gas+gas/4, snLedgerClaimGasFloor)
	balance, err := self.client.Balance(ctx, from)
	if err != nil {
		failed(snFailMessage(err))
		return
	}
	raw, maxCost, err := snSignClaim(ctx, self.client, self.chainId, self.settings.TxType, key, self.vault2, plan.data, gas)
	if err != nil {
		failed(snFailMessage(err))
		return
	}
	if balance.Cmp(maxCost) < 0 {
		failed(snNeedsGasMessage(maxCost, balance, snGasKeyInfo(key)))
		return
	}
	txHash, err := self.client.SendRawTransaction(ctx, raw)
	if err != nil {
		failed(snFailMessage(err))
		return
	}
	callback.Sent(plan.noId, plan.index, txHash)
	receipt, err := self.client.WaitReceipt(ctx, txHash, snClaimReceiptPoll, snClaimReceiptTimeout)
	if err != nil {
		failed(SnErrorCodeClaimFailed + ": " + err.Error())
		return
	}
	if !receipt.Status {
		// the reverted claim changed nothing, so the same call says why
		message := SnErrorCodeClaimFailed + ": the claim transaction reverted"
		if _, err := self.client.EthCallFrom(ctx, from, self.vault2, plan.data); err != nil {
			message = self.refusal(ctx, plan, err)
		}
		failed(message)
		return
	}
	amount, ok := self.claimedAmount(receipt, plan)
	if !ok {
		failed(SnErrorCodeClaimFailed + ": the claim transaction has no Claimed event for this account")
		return
	}
	callback.Confirmed(plan.noId, plan.index, txHash, amount)
}

// batcherVerified reports whether the configured batcher's code is the
// release claim batcher's. Any other code, or none, means claims go alone.
func (self *snLedgerClaimEngine) batcherVerified(ctx context.Context) bool {
	if !self.hasBatcher {
		return false
	}
	code, err := self.client.Code(ctx, self.batcher)
	return err == nil && 0 < len(code) && evm.Keccak256(code) == evm.ClaimBatcherRuntimeHash
}

// sizeBatch sizes a batch of plans for from to send through the batcher, as
// sn's onchain.SizeClaimBatch does: how many of them, from the first and at
// most snLedgerBatchMaxClaims, a transaction attempts at the most gas a batch
// names (the others wait to be claimed alone), the calldata claiming those, and
// the gas limit: the least within snLedgerBatchTolerance at which an eth_call
// attempts them all, with a fifth more, up to that most. The batcher attempts
// only the items its gas left covers, so a batch succeeds at any gas limit and
// eth_estimateGas cannot size it. from is the zero address for an external
// signer: neither the batcher nor vault2's claim depends on the sender.
func (self *snLedgerClaimEngine) sizeBatch(ctx context.Context, from [20]byte, plans []*snLedgerClaimPlan, gasPerClaim uint64) (int, []byte, uint64, error) {
	blockGasLimit, err := self.client.LatestGasLimit(ctx)
	if err != nil {
		return 0, nil, 0, fmt.Errorf("block gas limit: %w", err)
	}
	ceiling := snLedgerBatchCeiling(blockGasLimit)
	plans = plans[:min(len(plans), snLedgerBatchMaxClaims)]
	pack := func(plans []*snLedgerClaimPlan) ([]byte, error) {
		items := make([]*evm.LedgerClaim, len(plans))
		for i, plan := range plans {
			items[i] = plan.item(self.coldkey)
		}
		return evm.PackClaimMany(self.vault2, items, gasPerClaim)
	}
	attempted := func(data []byte, gas uint64) (uint64, error) {
		ret, err := self.client.EthCallGas(ctx, from, self.batcher, data, gas)
		if err != nil {
			return 0, err
		}
		return evm.DecodeClaimManyAttempted(ret)
	}
	data, err := pack(plans)
	if err != nil {
		return 0, nil, 0, err
	}
	fit, err := attempted(data, ceiling)
	if err != nil {
		return 0, nil, 0, fmt.Errorf("batch preflight at %d gas: %w", ceiling, err)
	}
	if fit == 0 || uint64(len(plans)) < fit {
		return 0, nil, 0, fmt.Errorf("a batch of %d claims attempts %d at %d gas", len(plans), fit, ceiling)
	}
	if fit < uint64(len(plans)) {
		if data, err = pack(plans[:fit]); err != nil {
			return 0, nil, 0, err
		}
	}
	low, high := uint64(0), ceiling
	for snLedgerBatchTolerance < high-low {
		middle := low + (high-low)/2
		count, err := attempted(data, middle)
		if ctx.Err() != nil {
			return 0, nil, 0, ctx.Err()
		}
		if err == nil && count == fit {
			high = middle
		} else {
			low = middle
		}
	}
	return int(fit), data, min(high+high/5, ceiling), nil
}

// sendBatch claims two or more plans in one batcher transaction from the gas
// key, and returns the plans to claim alone instead. A plan whose preflight
// vault2 refuses is reported failed and left out. Every passing plan is
// returned when the batch is not sent: no release batcher, fewer than two
// passing, a batch that cannot be sized, or a gas key that cannot pay for the
// batch, which a claim alone may still afford. Once the batch is sent its
// accounts are claimed only by it, except those its mined receipt shows it
// did not attempt, which are returned. The batch is prepared and sent within a
// claim's time, and its receipt waited for within another, so a slow
// preparation never cuts the wait short.
func (self *snLedgerClaimEngine) sendBatch(parent context.Context, key *secp.PrivateKey, plans []*snLedgerClaimPlan, callback SnLedgerClaimCallback) []*snLedgerClaimPlan {
	ctx, cancel := context.WithTimeout(parent, snClaimTimeout)
	defer cancel()
	if len(plans) < 2 || !self.batcherVerified(ctx) {
		return plans
	}
	from := evm.AddressOf(key)
	gasPerClaim := snLedgerClaimGasFloor
	var passing []*snLedgerClaimPlan
	for _, plan := range plans {
		if _, err := self.client.EthCallFrom(ctx, from, self.vault2, plan.data); err != nil {
			callback.Failed(plan.noId, plan.index, self.refusal(ctx, plan, err))
			continue
		}
		estimate, err := self.client.EstimateGas(ctx, from, self.vault2, plan.data, nil)
		if err != nil {
			callback.Failed(plan.noId, plan.index, self.refusal(ctx, plan, err))
			continue
		}
		// each item's cap covers its own estimate, as sn's claim tools give it
		gasPerClaim = max(gasPerClaim, estimate)
		passing = append(passing, plan)
	}
	if len(passing) < 2 {
		return passing
	}
	count, data, gas, err := self.sizeBatch(ctx, from, passing, gasPerClaim)
	if err != nil || count < 2 {
		return passing
	}
	batch, alone := passing[:count], passing[count:]
	balance, err := self.client.Balance(ctx, from)
	if err != nil {
		return passing
	}
	raw, maxCost, err := snSignClaim(ctx, self.client, self.chainId, self.settings.TxType, key, self.batcher, data, gas)
	if err != nil || balance.Cmp(maxCost) < 0 {
		return passing
	}
	failAll := func(message string) {
		for _, plan := range batch {
			callback.Failed(plan.noId, plan.index, message)
		}
	}
	txHash, err := self.client.SendRawTransaction(ctx, raw)
	if err != nil {
		// it may have reached the chain: none of its accounts is claimed again
		failAll(snFailMessage(err))
		return alone
	}
	for _, plan := range batch {
		callback.Sent(plan.noId, plan.index, txHash)
	}
	waitCtx, cancelWait := context.WithTimeout(parent, snClaimTimeout)
	defer cancelWait()
	receipt, err := self.client.WaitReceipt(waitCtx, txHash, snClaimReceiptPoll, snClaimReceiptTimeout)
	if err != nil {
		failAll(SnErrorCodeClaimFailed + ": " + err.Error())
		return alone
	}
	if !receipt.Status {
		failAll(SnErrorCodeClaimFailed + ": the batch transaction reverted")
		return alone
	}
	// vault2's Claimed event says what each account was paid, and the
	// batcher's events why an account was not
	outcomes, outcomesErr := evm.ClaimBatchOutcomes(receipt, self.batcher, len(batch))
	var notAttempted []*snLedgerClaimPlan
	for i, plan := range batch {
		if amount, ok := self.claimedAmount(receipt, plan); ok {
			callback.Confirmed(plan.noId, plan.index, txHash, amount)
			continue
		}
		switch {
		case outcomesErr != nil:
			callback.Failed(plan.noId, plan.index, SnErrorCodeClaimFailed+": the batch transaction has no Claimed event for this account, and its batcher events do not say why: "+outcomesErr.Error())
		case outcomes[i].Outcome == evm.ClaimBatchNotAttempted:
			notAttempted = append(notAttempted, plan)
		case outcomes[i].Outcome == evm.ClaimBatchRefused:
			callback.Failed(plan.noId, plan.index, self.batchRefusal(waitCtx, plan, outcomes[i].Selector))
		default:
			callback.Failed(plan.noId, plan.index, SnErrorCodeClaimFailed+": the batch transaction has no Claimed event for this account")
		}
	}
	return append(notAttempted, alone...)
}

// snLedgerLargestFirst orders accounts to claim alone in turn: the largest
// amount first, ties in the ledger's order, so a payer that can pay for fewer
// claims than there are accounts claims the most it can.
func snLedgerLargestFirst(plans []*snLedgerClaimPlan) []*snLedgerClaimPlan {
	ordered := slices.Clone(plans)
	slices.SortStableFunc(ordered, func(a *snLedgerClaimPlan, b *snLedgerClaimPlan) int {
		return cmp.Compare(b.amount, a.amount)
	})
	return ordered
}

// claim claims every account that has something to claim: two or more in one
// batch when they can be, and each other alone, in turn, the largest first. A
// gas key that cannot pay for the batch so claims the largest account first,
// and each account it then cannot pay for is reported needs_gas, nothing sent
// for it.
func (self *snLedgerClaimEngine) claim(ctx context.Context, key *secp.PrivateKey, callback SnLedgerClaimCallback) {
	refused := false
	planCtx, cancel := context.WithTimeout(ctx, snClaimsTimeout)
	plans, snErr := self.plans(planCtx, func(noId int64, index int64, message string) {
		refused = true
		callback.Failed(noId, index, message)
	})
	cancel()
	switch {
	case snErr != nil:
		callback.Failed(0, 0, snErr.Code+": "+snErr.Message)
	case len(plans) == 0 && !refused:
		callback.Failed(0, 0, snLedgerNothingToClaim)
	}
	if 2 <= len(plans) {
		plans = self.sendBatch(ctx, key, plans, callback)
	}
	for _, plan := range snLedgerLargestFirst(plans) {
		sendCtx, cancel := context.WithTimeout(ctx, snClaimTimeout)
		self.send(sendCtx, key, plan, callback)
		cancel()
	}
}

// unsignedTx is the plan's claim alone.
func (self *snLedgerClaimEngine) unsignedTx(plan *snLedgerClaimPlan) *SnLedgerUnsignedTx {
	items := NewSnLedgerClaimItemList()
	items.Add(plan.claimItem())
	return &SnLedgerUnsignedTx{
		NoId:      plan.noId,
		Index:     plan.index,
		RootIndex: plan.rootIndex,
		ChainId:   self.settings.ChainId,
		To:        self.vault2Hex(),
		Data:      "0x" + hex.EncodeToString(plan.data),
		Value:     "0",
		AmountRao: plan.amount,
		Gas:       int64(snLedgerClaimGasFloor),
		Vault:     self.vault2Hex(),
		Items:     items,
	}
}

// batchTx is the plans' claim through the batcher, with its calldata and its
// sized gas limit.
func (self *snLedgerClaimEngine) batchTx(plans []*snLedgerClaimPlan, data []byte, gas uint64) *SnLedgerUnsignedTx {
	items := NewSnLedgerClaimItemList()
	amount := int64(0)
	for _, plan := range plans {
		items.Add(plan.claimItem())
		amount += plan.amount
	}
	return &SnLedgerUnsignedTx{
		ChainId:   self.settings.ChainId,
		To:        "0x" + hex.EncodeToString(self.batcher[:]),
		Data:      "0x" + hex.EncodeToString(data),
		Value:     "0",
		AmountRao: amount,
		Gas:       int64(gas),
		Vault:     self.vault2Hex(),
		Batch:     true,
		Items:     items,
	}
}

func (self *snLedgerClaimPlan) claimItem() *SnLedgerClaimItem {
	return &SnLedgerClaimItem{
		NoId:      self.noId,
		Index:     self.index,
		RootIndex: self.rootIndex,
		Kind:      self.kind,
		Label:     self.label,
		AmountRao: self.amount,
	}
}

// unsignedTxs builds the transactions for an external signer to send, in
// order, that claim every account a claim pays now whose preflight passes:
// one batch of two or more through the release batcher when they can be, and
// each other alone, with snLedgerClaimGasFloor as its gas limit. An account is
// in one transaction only. With none the error is the first refusal (a pool
// whose ledger could not be read among them), else that there is nothing to
// claim.
func (self *snLedgerClaimEngine) unsignedTxs(ctx context.Context) (*SnLedgerUnsignedTxList, error) {
	list, _, err := self.claimTxs(ctx)
	return list, err
}

// claimTxs is unsignedTxs with the plans its transactions claim, in the
// ledger's order.
func (self *snLedgerClaimEngine) claimTxs(ctx context.Context) (*SnLedgerUnsignedTxList, []*snLedgerClaimPlan, error) {
	reason := ""
	refuse := func(noId int64, index int64, message string) {
		if reason == "" {
			reason = message
		}
	}
	plans, snErr := self.plans(ctx, refuse)
	if snErr != nil {
		return nil, nil, errors.New(snErr.Code + ": " + snErr.Message)
	}
	var passing []*snLedgerClaimPlan
	for _, plan := range plans {
		// the signer is not known; vault2's claim does not depend on it
		if _, err := self.client.EthCall(ctx, self.vault2, plan.data); err != nil {
			refuse(plan.noId, plan.index, self.refusal(ctx, plan, err))
			continue
		}
		passing = append(passing, plan)
	}
	list := NewSnLedgerUnsignedTxList()
	alone := passing
	if 2 <= len(passing) && self.batcherVerified(ctx) {
		count, data, gas, err := self.sizeBatch(ctx, [20]byte{}, passing, snLedgerClaimGasFloor)
		if err == nil && 2 <= count {
			list.Add(self.batchTx(passing[:count], data, gas))
			alone = passing[count:]
		}
	}
	for _, plan := range alone {
		list.Add(self.unsignedTx(plan))
	}
	if list.Len() == 0 {
		if reason == "" {
			reason = snLedgerNothingToClaim
		}
		return nil, nil, errors.New(reason)
	}
	return list, passing, nil
}

// SnLedgerClaimTransactionsResult is the claim transactions for an external
// wallet that pays their gas itself, checked against what it holds. A wallet
// that cannot pay for every transaction is given the largest account's claim
// alone, and the other accounts are StillClaimable: no transaction claims
// them, and a later claim does.
type SnLedgerClaimTransactionsResult struct {
	// the transactions to send, in order, each signed with exactly its Gas
	Transactions *SnLedgerUnsignedTxList `json:"transactions"`
	// the accounts claimable now that no transaction of Transactions claims,
	// because the payer cannot pay for them with the others, in the ledger's
	// order: Status SnLedgerClaimOutcomeStillClaimable, AmountRao what a claim
	// pays it now, and Message "needs_gas: detail". Empty when Transactions
	// claim every account
	StillClaimable *SnLedgerClaimOutcomeList `json:"still_claimable"`
	// lowercase 0x
	PayerAddress string `json:"payer_address"`
	// decimal wei
	PayerBalanceWei string `json:"payer_balance_wei"`
	// the price per gas the payer's balance is checked at, decimal wei: the
	// higher of the most an EIP-1559 wallet offers by default, twice the latest
	// base fee and the tip, and 1.1 × eth_gasPrice
	FeePerGasWei string `json:"fee_per_gas_wei"`
	// the most Transactions can cost: their gas limits × FeePerGasWei
	MaxCostWei string `json:"max_cost_wei"`
	// the most the transactions claiming every account can cost; MaxCostWei
	// when nothing is StillClaimable
	AllCostWei string `json:"all_cost_wei"`
	// the payer cannot pay even for Transactions, then the largest account's
	// claim alone: it needs MaxCostWei less PayerBalanceWei more. A wallet that
	// prices gas lower may still send it
	NeedsGas bool `json:"needs_gas"`
}

// feePerGas is the price per gas a payer's balance is checked at: the higher
// of the most an EIP-1559 wallet offers by default (ethers', and the device's
// with SnTxTypeEip1559: twice the latest base fee and the tip) and what a
// legacy one does (the device's 1.1 × eth_gasPrice). The wallet sets its own
// price, which is not known here; this is at least what common wallets offer.
func (self *snLedgerClaimEngine) feePerGas(ctx context.Context) (*big.Int, error) {
	gasPrice, err := self.client.GasPrice(ctx)
	if err != nil {
		return nil, err
	}
	fee := new(big.Int).Div(new(big.Int).Mul(gasPrice, big.NewInt(11)), big.NewInt(10))
	// as snSignClaim, a chain without a readable base fee prices legacy only
	if baseFee, err := self.client.LatestBaseFee(ctx); err == nil && baseFee != nil && baseFee.Sign() > 0 {
		tip, err := self.client.MaxPriorityFeePerGas(ctx)
		if err != nil || tip == nil {
			tip = big.NewInt(1_000_000_000)
		}
		feeCap := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(2)), tip)
		if fee.Cmp(feeCap) < 0 {
			fee = feeCap
		}
	}
	if fee.Sign() == 0 {
		fee = big.NewInt(1)
	}
	return fee, nil
}

// snLedgerTxsCost is the most the transactions can cost at fee a gas.
func snLedgerTxsCost(txs []*SnLedgerUnsignedTx, fee *big.Int) *big.Int {
	cost := big.NewInt(0)
	for _, tx := range txs {
		cost.Add(cost, new(big.Int).Mul(big.NewInt(tx.Gas), fee))
	}
	return cost
}

// unsignedTxsForPayer is unsignedTxs for a wallet at payer that pays the gas:
// every transaction when its balance pays for them all at feePerGas, else the
// largest account's claim alone, the others still claimable. No account is in
// two transactions.
func (self *snLedgerClaimEngine) unsignedTxsForPayer(ctx context.Context, payer [20]byte) (*SnLedgerClaimTransactionsResult, error) {
	list, passing, err := self.claimTxs(ctx)
	if err != nil {
		return nil, err
	}
	balance, err := self.client.Balance(ctx, payer)
	if err != nil {
		return nil, errors.New(snFailMessage(err))
	}
	fee, err := self.feePerGas(ctx)
	if err != nil {
		return nil, errors.New(snFailMessage(err))
	}
	allCost := snLedgerTxsCost(list.getAll(), fee)
	result := &SnLedgerClaimTransactionsResult{
		Transactions:    list,
		StillClaimable:  NewSnLedgerClaimOutcomeList(),
		PayerAddress:    "0x" + hex.EncodeToString(payer[:]),
		PayerBalanceWei: balance.String(),
		FeePerGasWei:    fee.String(),
		MaxCostWei:      allCost.String(),
		AllCostWei:      allCost.String(),
		NeedsGas:        balance.Cmp(allCost) < 0,
	}
	if !result.NeedsGas || len(passing) < 2 {
		// it pays for them all, or there is one account, whose claim alone
		// it is
		return result, nil
	}
	largest := snLedgerLargestFirst(passing)[0]
	alone := self.unsignedTx(largest)
	result.Transactions = NewSnLedgerUnsignedTxList()
	result.Transactions.Add(alone)
	maxCost := snLedgerTxsCost([]*SnLedgerUnsignedTx{alone}, fee)
	result.MaxCostWei = maxCost.String()
	result.NeedsGas = balance.Cmp(maxCost) < 0
	message := fmt.Sprintf("%s: claiming every account together needs up to %s of gas and the wallet %s holds %s, so only the largest account is claimed now; claim again for this one", SnErrorCodeNeedsGas, snFormatTao(allCost), result.PayerAddress, snFormatTao(balance))
	for _, plan := range passing {
		if plan == largest {
			continue
		}
		result.StillClaimable.Add(&SnLedgerClaimOutcome{
			NoId:      plan.noId,
			Index:     plan.index,
			Status:    SnLedgerClaimOutcomeStillClaimable,
			AmountRao: plan.amount,
			Message:   message,
		})
	}
	return result, nil
}

// device

func (self *snDevice) snLedgerClaimEngine() (*snLedgerClaimEngine, *SnError) {
	wallet := self.GetSnWallet()
	if wallet == nil {
		return nil, newSnError(SnErrorCodeConnectWalletFirst, "connect a Bittensor wallet first")
	}
	settings := self.GetSnChainSettings()
	api := self.api()
	fetch := func(ctx context.Context, requestUrl string) ([]byte, error) {
		return api.getHttpGetRaw()(ctx, requestUrl, "")
	}
	return newSnLedgerClaimEngine(settings, wallet.ColdkeySs58, self.snArtifactBaseUrl(settings), fetch)
}

// SnLedgerClaim claims every finalized v2 account of the connected coldkey
// that has something to claim, in every pool: two or more in one batcher
// transaction when they can be, else one transaction per account in turn, the
// largest first, signed with the device gas key; then Done. A gas key that
// can pay for one claim but not the batch so claims the largest account, and
// each other it then cannot pay for fails needs_gas, still claimable.
func (self *snDevice) SnLedgerClaim(callback SnLedgerClaimCallback) {
	go connect.HandleError(func() {
		defer callback.Done()
		engine, snErr := self.snLedgerClaimEngine()
		if snErr != nil {
			callback.Failed(0, 0, snErr.Code+": "+snErr.Message)
			return
		}
		key, err := self.snGasPrivateKey()
		if err != nil {
			callback.Failed(0, 0, SnErrorCodeLocalState+": "+err.Error())
			return
		}
		engine.claim(self.ctx, key, callback)
	})
}

// SnLedgerClaimTransactions builds the connected coldkey's v2 claim
// transactions for an external signer.
func (self *snDevice) SnLedgerClaimTransactions() (*SnLedgerUnsignedTxList, error) {
	engine, snErr := self.snLedgerClaimEngine()
	if snErr != nil {
		return nil, errors.New(snErr.Code + ": " + snErr.Message)
	}
	ctx, cancel := context.WithTimeout(self.ctx, snClaimsTimeout)
	defer cancel()
	return engine.unsignedTxs(ctx)
}

// SnLedgerClaimTransactionsForPayer builds the connected coldkey's v2 claim
// transactions for an external wallet whose EVM address payerAddress pays
// their gas: all of them when it holds enough, else the largest account's
// claim alone, with the others StillClaimable.
func (self *snDevice) SnLedgerClaimTransactionsForPayer(payerAddress string) (*SnLedgerClaimTransactionsResult, error) {
	payer, err := snLedgerPayer(payerAddress)
	if err != nil {
		return nil, err
	}
	engine, snErr := self.snLedgerClaimEngine()
	if snErr != nil {
		return nil, errors.New(snErr.Code + ": " + snErr.Message)
	}
	ctx, cancel := context.WithTimeout(self.ctx, snClaimsTimeout)
	defer cancel()
	return engine.unsignedTxsForPayer(ctx, payer)
}

// snLedgerPayer parses the EVM address of a wallet that pays for claims.
func snLedgerPayer(payerAddress string) ([20]byte, error) {
	payer, err := evm.ParseAddress(strings.TrimSpace(payerAddress))
	if err != nil {
		return [20]byte{}, fmt.Errorf("%s: the paying wallet %q is not an EVM address: %w", SnErrorCodeInvalidAddress, payerAddress, err)
	}
	return payer, nil
}

// hosts without a DeviceLocal (web)

// SnLedgerClaimTransactionsFor builds a coldkey's v2 claim transactions with
// explicit settings, for an injected browser wallet. The ledger is read from
// the settings' artifact host, else the default host.
func SnLedgerClaimTransactionsFor(settings *SnChainSettings, coldkeySs58 string) (*SnLedgerUnsignedTxList, error) {
	fetch := func(ctx context.Context, requestUrl string) ([]byte, error) {
		return snHttpGet(ctx, requestUrl, "the ledger account")
	}
	engine, snErr := newSnLedgerClaimEngine(settings, coldkeySs58, "", fetch)
	if snErr != nil {
		return nil, errors.New(snErr.Code + ": " + snErr.Message)
	}
	ctx, cancel := context.WithTimeout(context.Background(), snClaimsTimeout)
	defer cancel()
	return engine.unsignedTxs(ctx)
}

// SnLedgerClaimTransactionsForPayer builds a coldkey's v2 claim transactions,
// with explicit settings, for an injected browser wallet whose EVM address
// payerAddress pays their gas: SnLedgerClaimTransactionsFor's when its balance
// pays for them all at the chain's price, else the largest account's claim
// alone, with the other accounts StillClaimable; NeedsGas when it cannot pay
// even that. The ledger is read from the settings' artifact host, else the
// default host.
func SnLedgerClaimTransactionsForPayer(settings *SnChainSettings, coldkeySs58 string, payerAddress string) (*SnLedgerClaimTransactionsResult, error) {
	payer, err := snLedgerPayer(payerAddress)
	if err != nil {
		return nil, err
	}
	fetch := func(ctx context.Context, requestUrl string) ([]byte, error) {
		return snHttpGet(ctx, requestUrl, "the ledger account")
	}
	engine, snErr := newSnLedgerClaimEngine(settings, coldkeySs58, "", fetch)
	if snErr != nil {
		return nil, errors.New(snErr.Code + ": " + snErr.Message)
	}
	ctx, cancel := context.WithTimeout(context.Background(), snClaimsTimeout)
	defer cancel()
	return engine.unsignedTxsForPayer(ctx, payer)
}

// What a mined transaction of SnLedgerClaimTransactionsFor did for one of its
// accounts.
const (
	// vault2 paid it, AmountRao
	SnLedgerClaimOutcomeConfirmed = "confirmed"
	// vault2 did not pay it: Message is "<SnErrorCode*>: detail", and
	// SnErrorCodeAlreadyClaimed is success for the account
	SnLedgerClaimOutcomeFailed = "failed"
	// the batch ran short of gas before it: it is claimable as before, and the
	// transactions are built again to claim it
	SnLedgerClaimOutcomeNotAttempted = "not_attempted"
	// no transaction claims it now, because the paying wallet cannot pay for it
	// with the others (SnLedgerClaimTransactionsResult.StillClaimable): it is
	// claimable as before, and a later claim claims it
	SnLedgerClaimOutcomeStillClaimable = "still_claimable"
)

// SnLedgerClaimOutcome is one account's outcome in a mined claim transaction.
type SnLedgerClaimOutcome struct {
	NoId  int64 `json:"no_id"`
	Index int64 `json:"index"`
	// SnLedgerClaimOutcome*
	Status string `json:"status"`
	// what vault2's Claimed event paid, for confirmed; what a claim pays it
	// now, for still_claimable
	AmountRao int64  `json:"amount_rao"`
	Message   string `json:"message,omitempty"`
}

type SnLedgerClaimOutcomeList struct {
	exportedList[*SnLedgerClaimOutcome]
}

func NewSnLedgerClaimOutcomeList() *SnLedgerClaimOutcomeList {
	return &SnLedgerClaimOutcomeList{
		exportedList: *newExportedList[*SnLedgerClaimOutcome](),
	}
}

// A transaction receipt as JSON-RPC, ethers or viem give it: status is
// "0x1"/"0x0", 1/0, true/false or "success"/"reverted", or any of them as text.
type snReceiptJson struct {
	Status json.RawMessage `json:"status"`
	Logs   []struct {
		Address string   `json:"address"`
		Topics  []string `json:"topics"`
		Data    string   `json:"data"`
	} `json:"logs"`
}

// snParseReceipt reads a receipt's status and the logs whose address, topics
// and data parse.
func snParseReceipt(receiptJson string) (*evm.Receipt, error) {
	var answer snReceiptJson
	if err := json.Unmarshal([]byte(receiptJson), &answer); err != nil {
		return nil, fmt.Errorf("the receipt does not parse: %w", err)
	}
	receipt := &evm.Receipt{}
	var status any
	if err := json.Unmarshal(answer.Status, &status); err != nil {
		return nil, errors.New("the receipt has no status")
	}
	switch v := status.(type) {
	case bool:
		receipt.Status = v
	case float64:
		if v != 0 && v != 1 {
			return nil, fmt.Errorf("the receipt's status %v is not 0 or 1", v)
		}
		receipt.Status = v == 1
	case string:
		switch status := strings.ToLower(strings.TrimSpace(v)); status {
		case "success", "true":
			receipt.Status = true
		case "reverted", "false":
		default:
			// a quantity, "0x1" or "0x0"
			n, err := evm.ParseUint256(status)
			if err != nil || !n.IsUint64() || 1 < n.Uint64() {
				return nil, fmt.Errorf("the receipt's status %q is not a status", v)
			}
			receipt.Status = n.Uint64() == 1
		}
	default:
		return nil, errors.New("the receipt has no status")
	}
	for _, l := range answer.Logs {
		log := &evm.Log{}
		address, err := evm.ParseAddress(l.Address)
		data, dataErr := evm.ParseHexBytes(l.Data)
		for _, topic := range l.Topics {
			hash, topicErr := evm.ParseHash32(topic)
			err = errors.Join(err, topicErr)
			log.Topics = append(log.Topics, hash)
		}
		if err == nil && dataErr == nil {
			log.Address, log.Data = address, data
			receipt.Logs = append(receipt.Logs, log)
		}
	}
	return receipt, nil
}

// snClaimedIn is the amount of vault2's Claimed event for account (noId,
// index) in the receipt. A receipt's Claimed events are those of its own
// transaction's claims, so the pool and index name the account.
func snClaimedIn(receipt *evm.Receipt, vault2 [20]byte, noId int64, index int64) (int64, bool) {
	for _, log := range receipt.Logs {
		event, ok := evm.DecodeLedgerClaimed(log)
		if ok && log.Address == vault2 && event.NoId.Cmp(big.NewInt(noId)) == 0 &&
			event.Index.Cmp(big.NewInt(index)) == 0 && event.Amount.IsInt64() {
			return event.Amount.Int64(), true
		}
	}
	return 0, false
}

// snLedgerBatchRefusalOf names why the batcher's call of an item's claim
// reverted, from the first four bytes of its revert data, without the chain:
// a zero selector is a call that ran out of its gas cap, as one the staking
// precompile refuses does.
func snLedgerBatchRefusalOf(selector [4]byte) string {
	if selector == ([4]byte{}) {
		return SnErrorCodeClaimFailed + ": the staking precompile refused the transfer"
	}
	return snLedgerVaultRefusal(evm.DecodeRevert(selector[:]))
}

// SnLedgerClaimOutcomesOf reads what a mined transaction of
// SnLedgerClaimTransactionsFor did for each account it claims, in its Items'
// order: vault2's Claimed event confirms an account with the amount paid, and
// for a batch the batcher's events say why an account was not paid, or that it
// was not attempted. receiptJson is the transaction's receipt as JSON-RPC,
// ethers or viem give it. Pure: a page reads the receipt with its own wallet.
func SnLedgerClaimOutcomesOf(tx *SnLedgerUnsignedTx, receiptJson string) (*SnLedgerClaimOutcomeList, error) {
	if tx == nil || tx.Items == nil || tx.Items.Len() == 0 {
		return nil, errors.New("the transaction names no account")
	}
	vaultHex := tx.Vault
	if vaultHex == "" && !tx.Batch {
		vaultHex = tx.To
	}
	vault2, err := evm.ParseAddress(vaultHex)
	if err != nil {
		return nil, fmt.Errorf("vault: %w", err)
	}
	receipt, err := snParseReceipt(receiptJson)
	if err != nil {
		return nil, err
	}
	items := tx.Items.getAll()
	var batchOutcomes []*evm.ClaimBatchEvent
	var batchErr error
	if tx.Batch {
		batcher, err := evm.ParseAddress(tx.To)
		if err != nil {
			return nil, fmt.Errorf("batcher: %w", err)
		}
		batchOutcomes, batchErr = evm.ClaimBatchOutcomes(receipt, batcher, len(items))
	}
	outcomes := NewSnLedgerClaimOutcomeList()
	for i, item := range items {
		outcome := &SnLedgerClaimOutcome{NoId: item.NoId, Index: item.Index, Status: SnLedgerClaimOutcomeFailed}
		amount, paid := snClaimedIn(receipt, vault2, item.NoId, item.Index)
		switch {
		case !receipt.Status && tx.Batch:
			outcome.Message = SnErrorCodeClaimFailed + ": the batch transaction reverted"
		case !receipt.Status:
			outcome.Message = SnErrorCodeClaimFailed + ": the claim transaction reverted"
		case paid:
			outcome.Status, outcome.AmountRao = SnLedgerClaimOutcomeConfirmed, amount
		case !tx.Batch:
			outcome.Message = SnErrorCodeClaimFailed + ": the claim transaction has no Claimed event for this account"
		case batchErr != nil:
			outcome.Message = SnErrorCodeClaimFailed + ": the batch transaction has no Claimed event for this account, and its batcher events do not say why: " + batchErr.Error()
		case batchOutcomes[i].Outcome == evm.ClaimBatchNotAttempted:
			outcome.Status = SnLedgerClaimOutcomeNotAttempted
		case batchOutcomes[i].Outcome == evm.ClaimBatchRefused:
			outcome.Message = snLedgerBatchRefusalOf(batchOutcomes[i].Selector)
		default:
			outcome.Message = SnErrorCodeClaimFailed + ": the batch transaction has no Claimed event for this account"
		}
		outcomes.Add(outcome)
	}
	return outcomes, nil
}
