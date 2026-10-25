package sdk

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026/sn/evm"
	"github.com/urnetwork/sdk/v2026/sn/merkle"
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

// SnLedgerClaimCallback receives the progress of SnLedgerClaim. Failed
// messages start with an SnErrorCode* value followed by ": detail".
// SnErrorCodeAlreadyClaimed means another claim paid the account first,
// which is success for the account: the apps refresh. A failure that is not
// about one account (no wallet, v2 claims not configured, the ledger or the
// chain unreachable, nothing to claim) comes with noId and index 0.
type SnLedgerClaimCallback interface {
	Sent(noId int64, index int64, txHash string)
	Confirmed(noId int64, index int64, txHash string, amountRao int64)
	Failed(noId int64, index int64, message string)
	Done()
}

// SnLedgerUnsignedTx is a v2 claim transaction for an external signer.
type SnLedgerUnsignedTx struct {
	NoId      int64 `json:"no_id"`
	Index     int64 `json:"index"`
	RootIndex int64 `json:"root_index"`
	ChainId   int64 `json:"chain_id"`
	// Vault2Address, lowercase 0x
	To string `json:"to"`
	// 0x calldata of claim(noId, rootIndex, (index, coldkey, cumulative), proof)
	Data string `json:"data"`
	// decimal wei, always "0"
	Value string `json:"value"`
	// what the claim pays now: the cumulative less vault2's claimed
	AmountRao int64 `json:"amount_rao"`
	// the gas limit to sign with, snLedgerClaimGasFloor (2,600,000): a
	// signer's own estimate follows vault2's staking-hotkey list, which third
	// parties can lengthen before the claim is included
	Gas int64 `json:"gas"`
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
	settings    *SnChainSettings
	client      *evm.Client
	chainId     *big.Int
	vault2      [20]byte
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
	// the claim calldata
	data   []byte
	amount int64
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
	if baseUrl == "" {
		baseUrl = settings.ArtifactBaseUrl
	}
	return &snLedgerClaimEngine{
		settings:    settings,
		client:      settings.chainClient(),
		chainId:     big.NewInt(settings.ChainId),
		vault2:      vault2,
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
// reported to fail. An account only in the pending root is skipped, and so
// is one with nothing claimable in vault2 unless the ledger showed it with
// something claimable: then another claim paid it first.
func (self *snLedgerClaimEngine) plans(ctx context.Context, fail func(noId int64, index int64, message string)) ([]*snLedgerClaimPlan, *SnError) {
	entries, snErr := snReadLedger(ctx, self.fetch, self.baseUrl, self.coldkeySs58)
	if snErr != nil {
		return nil, snErr
	}
	var finalized []*snLedgerEntry
	var datas [][]byte
	for _, entry := range entries {
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
			noId:      account.NoId,
			index:     account.Index,
			rootIndex: entry.rootIndex,
			data:      data,
			amount:    account.FinalizedRao - claimed.Int64(),
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
		reason := self.runtimeRefusal(ctx, plan)
		if reason == "AmountTooLow" {
			return SnErrorCodeNotClaimable + ": AmountTooLow: the claim is below the runtime's transfer floor"
		}
		return SnErrorCodeClaimFailed + ": " + reason
	}
	switch reason := rpcErr.Reason(); reason {
	case "NothingToClaim":
		return SnErrorCodeAlreadyClaimed + ": NothingToClaim: another claim paid this account first"
	case "InvalidProof", "UnknownRoot":
		return SnErrorCodeProofMismatch + ": " + reason
	default:
		return SnErrorCodeClaimFailed + ": " + reason
	}
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

// claim sends one claim per account that has something to claim, in turn.
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
	for _, plan := range plans {
		sendCtx, cancel := context.WithTimeout(ctx, snClaimTimeout)
		self.send(sendCtx, key, plan, callback)
		cancel()
	}
}

// unsignedTxs builds a claim transaction for each account a claim pays now
// whose preflight passes, for an external signer, each with
// snLedgerClaimGasFloor as its gas limit. With none the error is the first
// account's refusal, else that there is nothing to claim.
func (self *snLedgerClaimEngine) unsignedTxs(ctx context.Context) (*SnLedgerUnsignedTxList, error) {
	reason := ""
	refuse := func(noId int64, index int64, message string) {
		if reason == "" {
			reason = message
		}
	}
	plans, snErr := self.plans(ctx, refuse)
	if snErr != nil {
		return nil, errors.New(snErr.Code + ": " + snErr.Message)
	}
	list := NewSnLedgerUnsignedTxList()
	for _, plan := range plans {
		// the signer is not known; vault2's claim does not depend on it
		if _, err := self.client.EthCall(ctx, self.vault2, plan.data); err != nil {
			refuse(plan.noId, plan.index, self.refusal(ctx, plan, err))
			continue
		}
		list.Add(&SnLedgerUnsignedTx{
			NoId:      plan.noId,
			Index:     plan.index,
			RootIndex: plan.rootIndex,
			ChainId:   self.settings.ChainId,
			To:        self.vault2Hex(),
			Data:      "0x" + hex.EncodeToString(plan.data),
			Value:     "0",
			AmountRao: plan.amount,
			Gas:       int64(snLedgerClaimGasFloor),
		})
	}
	if list.Len() == 0 {
		if reason == "" {
			reason = snLedgerNothingToClaim
		}
		return nil, errors.New(reason)
	}
	return list, nil
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
// that has something to claim, one transaction per account in turn, signed
// with the device gas key; then Done.
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
