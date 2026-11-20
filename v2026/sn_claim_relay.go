package sdk

// sn_claim_relay.go — everything the apps draw about a network's settlement v2
// claims. URnetwork claims for every consented coldkey and pays the gas (server
// controller/sn_claim_relay.go): automatically once a balance is worth 0.0125 TAO,
// and on a "Claim now" tap once it clears the transfer floor, at most once per
// coldkey per 30 days, counted from a paid claim URnetwork would not have sent by
// itself. The device's own claim with its gas key (sn_ledger_claim.go) stays as the
// fallback.
//
// SnClaimStatus reads what is pending and why, and chooses between the two, so an
// app only draws the answer: Action is what its main control does, and Reason says
// why a balance is still pending. Reasons and actions are stable values the apps
// localize. ShowsTile says whether a network with no wallet on this device shows
// the Settlement v2 tile at all, and ShowsClaimed whether the claim is drawn as
// claimed by URnetwork, with its transaction. A server that cannot answer leaves
// the fallback working: the balances come from the public ledger, and the action
// is the device's own claim. A page with no device, the web's wasm, gets the same
// rules over what it read with its own client (sn_claim_relay_js.go).
//
// From epoch 8 a coldkey has an account in each of the operator's pools, its
// pool and its capture the flag pool (sn CAPTURETHEFLAG.md §10). Accounts lists
// one row per (coldkey, pool), each with its pool's kind and label, and the top
// level is the primary coldkey's accounts together, so one action claims every
// pool: Claim now claims all of a coldkey's pools, and the device's own claim
// batches them. A pool whose ledger could not be read is listed in
// UnavailablePools and shown as unavailable, never as zero.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026/sn/evm"
)

// Why a balance is still pending.
const (
	// nothing to claim now; a root that is not final may hold more (PendingRao)
	SnClaimReasonNothingPending = "nothing_pending"
	// URnetwork claims it once it is worth the automatic threshold
	SnClaimReasonUnderThreshold = "under_threshold"
	// URnetwork claims it soon
	SnClaimReasonQueued = "queued"
	// today's budget for claims is spent; it waits for the next UTC day, which
	// costs nothing, since v2 credit never expires
	SnClaimReasonCapReached = "cap_reached"
	// its last claim was refused; URnetwork tries again later
	SnClaimReasonRefused = "refused"
	// URnetwork is not claiming: the device's own claim is the way for now
	SnClaimReasonRelayNotRunning = "relay_not_running"
	// URnetwork has paused claims for everyone for now: the device's own claim
	// is the way meanwhile
	SnClaimReasonRelayPaused = "relay_paused"
)

// Why Claim now cannot be tapped, beside the reasons above.
const (
	// the balance does not clear the transfer floor, under which a claim reverts
	SnClaimNowReasonUnderFloor = "under_floor"
	// already asked for, and URnetwork will send it
	SnClaimNowReasonRequested = "requested"
	// URnetwork paid a Claim now of this coldkey in the last 30 days: see
	// ClaimNowNextAtMillis
	SnClaimNowReasonLimit = "limit"
)

// What an app's main control does.
const (
	// nothing to claim
	SnClaimActionNone = "none"
	// URnetwork claims this; nothing to tap
	SnClaimActionWait = "wait"
	// SnClaimNow asks URnetwork to claim it now
	SnClaimActionClaimNow = "claim_now"
	// SnLedgerClaim claims it with the device's gas key
	SnClaimActionSelfClaim = "self_claim"
	// the device's own claim needs gas first: GetSnGasKey is where to send it
	SnClaimActionTopUpGas = "top_up_gas"
)

// SnClaimRelayStateUnavailable is the relay's state when the server did not
// answer. The server's own states are its status field's.
const SnClaimRelayStateUnavailable = "unavailable"

const snClaimStatusTimeout = 60 * time.Second

// SnClaimRelayAccount is one coldkey's account in one of the operator's pools,
// as the server's relay sees it. Amounts are rao of α unless they say
// otherwise.
type SnClaimRelayAccount struct {
	ColdkeySs58 string `json:"coldkey_ss58"`
	NoId        int64  `json:"no_id"`
	// SnLedgerPoolKind*: the relay's, else a capture the flag pool's id is ctf
	// and any other a pool
	Kind string `json:"kind"`
	// the relay's English name for the pool, else "Pool <no_id>" or "Capture
	// the flag", for an app without a localized one
	Label string `json:"label"`
	Index int64  `json:"index"`
	// what a claim would pay now
	ClaimableRao int64 `json:"claimable_rao"`
	// its TAO value, in rao of TAO, at the relay's last price; 0 without one
	ValueTaoRao int64 `json:"value_tao_rao"`
	// held by a root that is not final yet, on top of the claimable
	PendingRao int64 `json:"pending_rao"`
	// SnClaimReason*
	Reason            string `json:"reason"`
	ClaimNowAvailable bool   `json:"claim_now_available"`
	// why not, when not: SnClaimNowReason* or an SnClaimReason*
	ClaimNowReason string `json:"claim_now_reason,omitempty"`
	// when this coldkey's Claim now limit ends, unix milliseconds; 0 when none holds
	ClaimNowNextAtMillis int64 `json:"claim_now_next_at_millis,omitempty"`
	// the transaction of this account's latest claim when URnetwork sent it, and
	// what it paid: "" and 0 for a claim anyone else sent, or none
	ClaimTxHash    string `json:"claim_tx_hash,omitempty"`
	ClaimAmountRao int64  `json:"claim_amount_rao,omitempty"`
	// Set by the sdk, not the server:
	// the account of the connected wallet, the one SnLedgerClaim claims
	Connected bool `json:"connected,omitempty"`
	// SnClaimAction*
	Action string `json:"action,omitempty"`
	// the explorer's page of ClaimTxHash, from the device's chain settings; ""
	// without one
	ClaimTxUrl string `json:"claim_tx_url,omitempty"`
}

type SnClaimRelayAccountList struct {
	exportedList[*SnClaimRelayAccount]
}

func NewSnClaimRelayAccountList() *SnClaimRelayAccountList {
	return &SnClaimRelayAccountList{
		exportedList: *newExportedList[*SnClaimRelayAccount](),
	}
}

type SnClaimRelayError struct {
	Message string `json:"message"`
}

// SnClaimRelayResult is GET /sn/claim-relay?partial=true. With an Error the
// balances could not be read: they are unavailable, never zero. With
// partial=true a pool whose accounts the relay cannot read is named in
// UnavailablePools while the others are answered. Without it the server leaves
// out a capture the flag pool it cannot read, answering the operator's pool and
// the relay's State as before that pool existed, and answers an Error for any
// other pool it cannot read, so a client that predates the field never takes a
// partial list of the other pools for a whole one. A server that predates the
// parameter ignores it.
type SnClaimRelayResult struct {
	// no_key means no relayer key is configured yet; not_running that the relay
	// has no recent pass
	State string `json:"state"`
	// in rao of TAO
	AutomaticThresholdTaoRao int64 `json:"automatic_threshold_tao_rao"`
	TransferFloorTaoRao      int64 `json:"transfer_floor_tao_rao"`
	// the same in rao of α at the relay's last price, 0 without one
	AutomaticThresholdRao int64 `json:"automatic_threshold_rao"`
	TransferFloorRao      int64 `json:"transfer_floor_rao"`
	// days between two Claim now of one coldkey
	ClaimNowPeriodDays int64                    `json:"claim_now_period_days"`
	Accounts           *SnClaimRelayAccountList `json:"accounts"`
	// the pools whose accounts the relay could not read, each shown as
	// unavailable; empty for a whole answer
	UnavailablePools *SnLedgerUnavailablePoolList `json:"unavailable_pools"`
	Error            *SnClaimRelayError           `json:"error,omitempty"`
}

type SnClaimRelayCallback connect.ApiCallback[*SnClaimRelayResult]

// snClaimRelayPath asks for a partial answer: a pool the relay cannot read is
// named, and the others are answered.
const snClaimRelayPath = "/sn/claim-relay?partial=true"

// Reads the relay's answer for the session's network.
func (self *Api) SnClaimRelay(callback SnClaimRelayCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			self.apiUrl+snClaimRelayPath,
			self.GetByJwt(),
			&SnClaimRelayResult{},
			callback,
		)
	})
}

//gomobile:noexport
func (self *Api) SnClaimRelaySyncWithContext(ctx context.Context) (*SnClaimRelayResult, error) {
	return connect.HttpGetWithRawFunction(
		ctx,
		self.getHttpGetRaw(),
		self.apiUrl+snClaimRelayPath,
		self.GetByJwt(),
		&SnClaimRelayResult{},
		connect.NewNoopApiCallback[*SnClaimRelayResult](),
	)
}

// SnClaimRelayClaimNowArgs names one of the session's network's coldkeys.
type SnClaimRelayClaimNowArgs struct {
	ColdkeySs58 string `json:"coldkey_ss58"`
}

// SnClaimRelayClaimNowResult says whether the tap was taken. Not requested, Reason
// says why not, as SnClaimRelayAccount.ClaimNowReason does.
type SnClaimRelayClaimNowResult struct {
	Requested bool   `json:"requested"`
	Reason    string `json:"reason,omitempty"`
	// when this coldkey's limit ends, unix milliseconds, for a tap the limit
	// refused
	NextAtMillis int64              `json:"next_at_millis,omitempty"`
	Error        *SnClaimRelayError `json:"error,omitempty"`
}

type SnClaimRelayClaimNowCallback connect.ApiCallback[*SnClaimRelayClaimNowResult]

// Asks the relay to claim one of the session's network's coldkeys now.
func (self *Api) SnClaimRelayClaimNow(args *SnClaimRelayClaimNowArgs, callback SnClaimRelayClaimNowCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/sn/claim-relay/claim-now", self.apiUrl),
			args,
			self.GetByJwt(),
			&SnClaimRelayClaimNowResult{},
			callback,
		)
	})
}

//gomobile:noexport
func (self *Api) SnClaimRelayClaimNowSyncWithContext(ctx context.Context, args *SnClaimRelayClaimNowArgs) (*SnClaimRelayClaimNowResult, error) {
	return connect.HttpPostWithRawFunction(
		ctx,
		self.getHttpPostRaw(),
		fmt.Sprintf("%s/sn/claim-relay/claim-now", self.apiUrl),
		args,
		self.GetByJwt(),
		&SnClaimRelayClaimNowResult{},
		connect.NewNoopApiCallback[*SnClaimRelayClaimNowResult](),
	)
}

// SnClaimStatusResult is everything an app draws. The top level is the primary
// coldkey's accounts together, one per pool: the connected wallet's when it has
// one, else the first with something to claim. Accounts lists each of the
// network's, one per coldkey and pool, including a wallet's it replaced, whose
// balance URnetwork keeps claiming; UnavailablePools each pool that could not be
// read. With an Error the balances are unavailable, never zero.
type SnClaimStatusResult struct {
	// the relay's state, SnClaimRelayStateUnavailable when the server did not answer
	RelayState  string `json:"relay_state"`
	ColdkeySs58 string `json:"coldkey_ss58,omitempty"`
	// SnClaimAction*: what the main control does, for every pool of the
	// primary coldkey
	Action string `json:"action"`
	// SnClaimReason*: why a balance is pending, the lead account's: the one
	// with the most to claim
	Reason string `json:"reason"`
	// the primary coldkey's accounts together
	ClaimableRao int64 `json:"claimable_rao"`
	ValueTaoRao  int64 `json:"value_tao_rao"`
	PendingRao   int64 `json:"pending_rao"`
	// whether SnClaimNow can be tapped, why not, and when its limit ends
	ClaimNowAvailable    bool   `json:"claim_now_available"`
	ClaimNowReason       string `json:"claim_now_reason,omitempty"`
	ClaimNowNextAtMillis int64  `json:"claim_now_next_at_millis,omitempty"`
	// the fallback: SnLedgerClaim can claim this now with the gas key, whatever
	// Action says, and whether the gas key is short of the gas that takes
	SelfClaimAvailable bool `json:"self_claim_available"`
	SelfClaimNeedsGas  bool `json:"self_claim_needs_gas"`
	// what the policy says, in rao of α at the relay's last price, 0 without one
	AutomaticThresholdRao int64 `json:"automatic_threshold_rao"`
	TransferFloorRao      int64 `json:"transfer_floor_rao"`
	// the same in rao of TAO, which is what the policy sets: "once it's worth about 0.0125 TAO"
	AutomaticThresholdTaoRao int64 `json:"automatic_threshold_tao_rao"`
	TransferFloorTaoRao      int64 `json:"transfer_floor_tao_rao"`
	// days between two Claim now of one coldkey
	ClaimNowPeriodDays int64 `json:"claim_now_period_days"`
	// the Settlement v2 tile shows for this status: it was read, and the primary
	// account has a claimable or pending balance, or shows the claimed state, which
	// is reached from the tile. A device whose own wallet's ledger has a tile shows
	// that one, as before the relay; otherwise the tile shows exactly while this is
	// true
	ShowsTile bool `json:"shows_tile"`
	// the claimed state, "Claimed by URnetwork" with the transaction: URnetwork's
	// claim is the lead account's latest, nothing is claimable now in any of the
	// primary coldkey's pools, and none is unavailable
	ShowsClaimed bool `json:"shows_claimed"`
	// the lead account's latest claim when URnetwork sent it: the transaction,
	// the explorer's page of it ("" without one) and what it paid the primary
	// coldkey's accounts together, as a batch pays several
	ClaimTxHash    string                   `json:"claim_tx_hash,omitempty"`
	ClaimTxUrl     string                   `json:"claim_tx_url,omitempty"`
	ClaimAmountRao int64                    `json:"claim_amount_rao,omitempty"`
	Accounts       *SnClaimRelayAccountList `json:"accounts"`
	// the pools that could not be read, each shown as unavailable beside the
	// accounts; empty for a whole answer
	UnavailablePools *SnLedgerUnavailablePoolList `json:"unavailable_pools"`
	// UnavailablePools is not empty: the sums leave those pools out
	Partial bool     `json:"partial"`
	Error   *SnError `json:"error,omitempty"`
}

type SnClaimStatusCallback connect.ApiCallback[*SnClaimStatusResult]

// SnClaimNowResult is the answer to a Claim now tap.
type SnClaimNowResult struct {
	// taken: URnetwork sends it first, under the same daily budget
	Requested bool `json:"requested"`
	// why not, as SnClaimStatusResult.ClaimNowReason
	Reason string `json:"reason,omitempty"`
	// when this coldkey's limit ends, unix milliseconds, for a tap the limit
	// refused
	NextAtMillis int64    `json:"next_at_millis,omitempty"`
	Error        *SnError `json:"error,omitempty"`
}

type SnClaimNowCallback connect.ApiCallback[*SnClaimNowResult]

// snClaimRelayUsable reports a reason under which URnetwork is still the one to
// claim: it is claiming, or it will once a balance or a new day allows.
func snClaimRelayUsable(reason string) bool {
	switch reason {
	case SnClaimReasonRelayNotRunning, SnClaimReasonRelayPaused:
		return false
	}
	return true
}

// snClaimAction chooses what the main control does for one account: the relay
// whenever it is claiming, and the device's own claim only when it is not, since
// that spends TAO the user has to hold. Pure, so every app gets the same choice.
func snClaimAction(account *SnClaimRelayAccount, selfClaimAvailable bool, selfClaimNeedsGas bool) string {
	switch {
	case account == nil || account.ClaimableRao <= 0:
		return SnClaimActionNone
	case snClaimRelayUsable(account.Reason):
		if account.ClaimNowAvailable {
			return SnClaimActionClaimNow
		}
		return SnClaimActionWait
	case !selfClaimAvailable:
		return SnClaimActionWait
	case selfClaimNeedsGas:
		return SnClaimActionTopUpGas
	}
	return SnClaimActionSelfClaim
}

// snClaimStatusPrimary picks the account whose coldkey the top level describes:
// the connected wallet's before any other, and one with something to claim before
// one without.
func snClaimStatusPrimary(accounts []*SnClaimRelayAccount) *SnClaimRelayAccount {
	rank := func(account *SnClaimRelayAccount) int {
		value := 0
		if account.Connected {
			value += 2
		}
		if 0 < account.ClaimableRao {
			value += 1
		}
		return value
	}
	var primary *SnClaimRelayAccount
	for _, account := range accounts {
		if primary == nil || rank(primary) < rank(account) {
			primary = account
		}
	}
	return primary
}

// snClaimStatusLead picks the account of a coldkey whose reason and claim the top
// level names: the one with the most to claim, then one URnetwork claimed, then
// the first.
func snClaimStatusLead(group []*SnClaimRelayAccount) *SnClaimRelayAccount {
	var lead *SnClaimRelayAccount
	for _, account := range group {
		switch {
		case lead == nil, lead.ClaimableRao < account.ClaimableRao:
			lead = account
		case lead.ClaimableRao == account.ClaimableRao && lead.ClaimTxHash == "" && account.ClaimTxHash != "":
			lead = account
		}
	}
	return lead
}

// snRelayPoolKind is a relay account's kind: the relay's, or, from a relay that
// names none, a capture the flag pool's id is ctf and any other a pool.
func snRelayPoolKind(kind string, noId int64) string {
	if kind == "" {
		return snLedgerPoolKind(nil, noId)
	}
	return snLedgerPoolKind(&kind, noId)
}

// snClaimStatusBuild turns the server's answer and the device's gas key into what
// an app draws. relay is nil when the server did not answer. Pure.
func snClaimStatusBuild(relay *SnClaimRelayResult, connectedSs58 string, selfClaimConfigured bool, selfClaimNeedsGas bool) *SnClaimStatusResult {
	result := &SnClaimStatusResult{
		RelayState:       SnClaimRelayStateUnavailable,
		Action:           SnClaimActionNone,
		Reason:           SnClaimReasonNothingPending,
		Accounts:         NewSnClaimRelayAccountList(),
		UnavailablePools: NewSnLedgerUnavailablePoolList(),
	}
	accounts := []*SnClaimRelayAccount{}
	if relay != nil {
		result.RelayState = relay.State
		result.AutomaticThresholdRao = relay.AutomaticThresholdRao
		result.TransferFloorRao = relay.TransferFloorRao
		result.AutomaticThresholdTaoRao = relay.AutomaticThresholdTaoRao
		result.TransferFloorTaoRao = relay.TransferFloorTaoRao
		result.ClaimNowPeriodDays = relay.ClaimNowPeriodDays
		if relay.Accounts != nil {
			for _, account := range relay.Accounts.getAll() {
				if account != nil {
					accounts = append(accounts, account)
				}
			}
		}
		if relay.UnavailablePools != nil {
			for _, pool := range relay.UnavailablePools.getAll() {
				if pool == nil {
					continue
				}
				pool.Kind = snRelayPoolKind(pool.Kind, pool.NoId)
				pool.Label = snLedgerPoolLabel(&pool.Label, pool.Kind, pool.NoId)
				result.UnavailablePools.Add(pool)
			}
		}
	}
	result.Partial = 0 < result.UnavailablePools.Len()
	for _, account := range accounts {
		account.Kind = snRelayPoolKind(account.Kind, account.NoId)
		account.Label = snLedgerPoolLabel(&account.Label, account.Kind, account.NoId)
		account.Connected = connectedSs58 != "" && account.ColdkeySs58 == connectedSs58
		account.Action = snClaimAction(account, snClaimSelfClaimAvailable(relay, account, selfClaimConfigured), selfClaimNeedsGas)
		// a transaction the sdk cannot name is none
		if !snClaimTxHashPattern.MatchString(account.ClaimTxHash) {
			account.ClaimTxHash = ""
		}
		if account.ClaimTxHash == "" || account.ClaimAmountRao < 0 {
			account.ClaimAmountRao = 0
		}
		result.Accounts.Add(account)
	}
	primary := snClaimStatusPrimary(accounts)
	if primary == nil {
		result.ColdkeySs58 = connectedSs58
		// an unavailable pool is shown as unavailable, never left out
		result.ShowsTile = result.Partial
		return result
	}
	// the primary coldkey's accounts, one per pool: one action claims them all
	var group []*SnClaimRelayAccount
	for _, account := range accounts {
		if account.ColdkeySs58 == primary.ColdkeySs58 {
			group = append(group, account)
		}
	}
	lead := snClaimStatusLead(group)
	result.ColdkeySs58 = primary.ColdkeySs58
	result.Reason = lead.Reason
	for _, account := range group {
		result.ClaimableRao += account.ClaimableRao
		result.ValueTaoRao += account.ValueTaoRao
		result.PendingRao += account.PendingRao
		// Claim now claims every pool of the coldkey, under one limit
		result.ClaimNowAvailable = result.ClaimNowAvailable || account.ClaimNowAvailable
		result.ClaimNowNextAtMillis = max(result.ClaimNowNextAtMillis, account.ClaimNowNextAtMillis)
		// the device's own claim claims every pool of the connected wallet
		result.SelfClaimAvailable = result.SelfClaimAvailable || snClaimSelfClaimAvailable(relay, account, selfClaimConfigured)
		if lead.ClaimTxHash != "" && account.ClaimTxHash == lead.ClaimTxHash {
			result.ClaimAmountRao += account.ClaimAmountRao
		}
	}
	if !result.ClaimNowAvailable {
		result.ClaimNowReason = lead.ClaimNowReason
	}
	result.SelfClaimNeedsGas = result.SelfClaimAvailable && selfClaimNeedsGas
	result.Action = snClaimAction(&SnClaimRelayAccount{
		ClaimableRao:      result.ClaimableRao,
		Reason:            lead.Reason,
		ClaimNowAvailable: result.ClaimNowAvailable,
	}, result.SelfClaimAvailable, selfClaimNeedsGas)
	result.ClaimTxHash = lead.ClaimTxHash
	result.ShowsClaimed = lead.ClaimTxHash != "" && result.ClaimableRao <= 0 && !result.Partial
	result.ShowsTile = snClaimStatusShowsTile(&SnClaimRelayAccount{
		ClaimableRao: result.ClaimableRao,
		PendingRao:   result.PendingRao,
	}, result.ShowsClaimed) || result.Partial
	return result
}

// A transaction hash as the relay names it.
var snClaimTxHashPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)

// snClaimStatusShowsTile is when a network with no wallet on this device shows the
// Settlement v2 tile: while the primary coldkey's accounts, given together, have
// a claimable or pending balance, as Apple's Earnings first drew it, and while
// they show the claimed state, so that a balance URnetwork claimed in full stays
// reachable with its transaction.
func snClaimStatusShowsTile(primary *SnClaimRelayAccount, showsClaimed bool) bool {
	return primary != nil && (0 < primary.ClaimableRao || 0 < primary.PendingRao || showsClaimed)
}

// snClaimStatusLinkTransactions names the explorer's page of each claim's
// transaction, from the device's chain settings. Pure.
func snClaimStatusLinkTransactions(result *SnClaimStatusResult, settings *SnChainSettings) {
	if result.Accounts != nil {
		for _, account := range result.Accounts.getAll() {
			account.ClaimTxUrl = settings.ExplorerUrlForTx(account.ClaimTxHash)
		}
	}
	result.ClaimTxUrl = settings.ExplorerUrlForTx(result.ClaimTxHash)
}

// The device's own claim can only be of the connected wallet's account, on a
// device whose chain settings can send one, for a balance the relay's last price
// does not put under the floor. Without a price the claim's own preflight decides.
func snClaimSelfClaimAvailable(relay *SnClaimRelayResult, account *SnClaimRelayAccount, configured bool) bool {
	if !configured || !account.Connected || account.ClaimableRao <= 0 {
		return false
	}
	if relay != nil && 0 < relay.TransferFloorRao && account.ClaimableRao < relay.TransferFloorRao {
		return false
	}
	return true
}

// The most a claim signed now pays per gas, as snSignClaim prices it.
func snClaimMaxFeePerGas(ctx context.Context, client *evm.Client, txType string) (*big.Int, error) {
	if txType == SnTxTypeEip1559 {
		if baseFee, err := client.LatestBaseFee(ctx); err == nil && baseFee != nil && baseFee.Sign() > 0 {
			tip, err := client.MaxPriorityFeePerGas(ctx)
			if err != nil || tip == nil {
				tip = big.NewInt(1_000_000_000)
			}
			feeCap := new(big.Int).Mul(baseFee, big.NewInt(2))
			return feeCap.Add(feeCap, tip), nil
		}
	}
	gasPrice, err := client.GasPrice(ctx)
	if err != nil {
		return nil, err
	}
	gasPrice = new(big.Int).Div(new(big.Int).Mul(gasPrice, big.NewInt(11)), big.NewInt(10))
	if gasPrice.Sign() == 0 {
		gasPrice = big.NewInt(1)
	}
	return gasPrice, nil
}

// Whether the gas key holds what a self-claim's gas limit takes at the chain's
// price. False when it cannot be read: the claim itself then says needs_gas.
func (self *snDevice) snClaimSelfClaimNeedsGas(ctx context.Context) bool {
	settings := self.GetSnChainSettings()
	key, err := self.snGasPrivateKey()
	if err != nil || settings == nil || settings.validateLedgerClaims() != nil {
		return false
	}
	client := settings.chainClient()
	feePerGas, err := snClaimMaxFeePerGas(ctx, client, settings.TxType)
	if err != nil {
		return false
	}
	balance, err := client.Balance(ctx, evm.AddressOf(key))
	if err != nil {
		return false
	}
	return balance.Cmp(new(big.Int).Mul(feePerGas, new(big.Int).SetUint64(snLedgerClaimGasFloor))) < 0
}

// snClaimStatus reads and builds the status. fallback reads the public ledger when
// the relay's answer is not available.
func (self *snDevice) snClaimStatus(ctx context.Context) *SnClaimStatusResult {
	connectedSs58 := ""
	if wallet := self.GetSnWallet(); wallet != nil {
		connectedSs58 = strings.TrimSpace(wallet.ColdkeySs58)
	}
	settings := self.GetSnChainSettings()
	selfClaimConfigured := settings != nil && settings.validateLedgerClaims() == nil && self.GetSnGasKey() != nil

	relay, err := self.api().SnClaimRelaySyncWithContext(ctx)
	return snClaimStatusOf(
		relay,
		err,
		connectedSs58,
		selfClaimConfigured,
		func() (*SnClaimRelayAccountList, *SnLedgerUnavailablePoolList, *SnError) {
			return self.snClaimLedgerAccounts(ctx, settings, connectedSs58)
		},
		func() bool {
			return self.snClaimSelfClaimNeedsGas(ctx)
		},
		settings,
	)
}

// snClaimStatusOf builds the status from what was read, on a device and on a page
// with no device (sn_claim_relay_js.go), so both follow the same rules. relay is
// the server's answer, nil when it did not answer, with relayErr saying why.
// ledger reads the connected wallet's accounts from the public ledger, which stand
// in for an answer without balances, with the pools it could not read; it is
// called only then. needsGas says whether
// the device's own claim is short of gas, asked only when that claim could pay
// something; nil is never short. settings links each claim's transaction; nil
// links none. Pure apart from what ledger and needsGas read.
func snClaimStatusOf(
	relay *SnClaimRelayResult,
	relayErr error,
	connectedSs58 string,
	selfClaimConfigured bool,
	ledger func() (*SnClaimRelayAccountList, *SnLedgerUnavailablePoolList, *SnError),
	needsGas func() bool,
	settings *SnChainSettings,
) *SnClaimStatusResult {
	var relayError *SnError
	switch {
	case relayErr != nil || relay == nil:
		relay = nil
		message := "the claim relay did not answer"
		if relayErr != nil {
			message = relayErr.Error()
		}
		relayError = newSnError(SnErrorCodeLedgerUnavailable, message)
	case relay.Error != nil:
		relayError = newSnError(SnErrorCodeLedgerUnavailable, relay.Error.Message)
	}
	if relayError != nil && connectedSs58 != "" && ledger != nil {
		// The relay's answer has no balances. The public ledger still has the
		// connected wallet's, and the device's own claim still works.
		state := SnClaimRelayStateUnavailable
		if relay != nil {
			state = relay.State
		}
		accounts, unavailable, snErr := ledger()
		if snErr == nil {
			// a partial read carries its unavailable pools, so it is never
			// drawn as whole
			relay = &SnClaimRelayResult{State: state, Accounts: accounts, UnavailablePools: unavailable}
			relayError = nil
		} else {
			relayError = snErr
		}
	}
	if relayError != nil {
		// unavailable, never zero: no balance, so no tile and nothing claimed
		result := snClaimStatusBuild(nil, connectedSs58, selfClaimConfigured, false)
		if relay != nil {
			result.RelayState = relay.State
		}
		result.Error = relayError
		return result
	}
	gasShort := false
	if connectedSs58 != "" && selfClaimConfigured && needsGas != nil && snClaimAnyClaimable(relay, connectedSs58) {
		gasShort = needsGas()
	}
	result := snClaimStatusBuild(relay, connectedSs58, selfClaimConfigured, gasShort)
	snClaimStatusLinkTransactions(result, settings)
	return result
}

func snClaimAnyClaimable(relay *SnClaimRelayResult, coldkeySs58 string) bool {
	if relay == nil || relay.Accounts == nil {
		return false
	}
	for _, account := range relay.Accounts.getAll() {
		if account != nil && account.ColdkeySs58 == coldkeySs58 && 0 < account.ClaimableRao {
			return true
		}
	}
	return false
}

// The connected wallet's accounts from the public ledger, with no relay behind
// them: the reason says so, and the app falls back to the device's own claim.
func (self *snDevice) snClaimLedgerAccounts(ctx context.Context, settings *SnChainSettings, coldkeySs58 string) (*SnClaimRelayAccountList, *SnLedgerUnavailablePoolList, *SnError) {
	api := self.api()
	fetch := func(ctx context.Context, requestUrl string) ([]byte, error) {
		return api.getHttpGetRaw()(ctx, requestUrl, "")
	}
	return snClaimLedgerRelayAccounts(snLedgerAccounts(ctx, fetch, self.snArtifactBaseUrl(settings), coldkeySs58), coldkeySs58)
}

// snClaimLedgerRelayAccounts turns a read of the connected wallet's accounts in the
// public ledger into the relay's accounts, with no relay behind them, and the
// pools it could not read. A read that was not made or has no accounts is the
// ledger unavailable, never zero. Pure.
func snClaimLedgerRelayAccounts(ledger *SnLedgerAccountsResult, coldkeySs58 string) (*SnClaimRelayAccountList, *SnLedgerUnavailablePoolList, *SnError) {
	switch {
	case ledger != nil && ledger.Error != nil:
		code, message := ledger.Error.Code, ledger.Error.Message
		if code == "" {
			code = SnErrorCodeLedgerUnavailable
		}
		if message == "" {
			message = "the ledger could not be read"
		}
		return nil, nil, newSnError(code, message)
	case ledger == nil || ledger.Accounts == nil:
		return nil, nil, newSnError(SnErrorCodeLedgerUnavailable, "the ledger could not be read")
	}
	unavailable := NewSnLedgerUnavailablePoolList()
	if ledger.UnavailablePools != nil {
		for _, pool := range ledger.UnavailablePools.getAll() {
			if pool != nil {
				unavailable.Add(pool)
			}
		}
	}
	if ledger.Partial && unavailable.Len() == 0 {
		// a read that says it is partial without naming what it lacks
		return nil, nil, newSnError(SnErrorCodeLedgerUnavailable, "the ledger answer is partial and names no unavailable pool")
	}
	accounts := NewSnClaimRelayAccountList()
	for _, account := range ledger.Accounts.getAll() {
		if account == nil {
			continue
		}
		reason := SnClaimReasonRelayNotRunning
		if account.ClaimableRao <= 0 {
			reason = SnClaimReasonNothingPending
		}
		pending := int64(0)
		if account.Pending && account.FinalizedRao < account.PendingRao {
			pending = account.PendingRao - account.FinalizedRao
		}
		accounts.Add(&SnClaimRelayAccount{
			ColdkeySs58:    coldkeySs58,
			NoId:           account.NoId,
			Kind:           account.Kind,
			Label:          account.Label,
			Index:          account.Index,
			ClaimableRao:   account.ClaimableRao,
			PendingRao:     pending,
			Reason:         reason,
			ClaimNowReason: reason,
		})
	}
	return accounts, unavailable, nil
}

// SnClaimStatus reads what is pending and why, and what the main control does.
func (self *snDevice) SnClaimStatus(callback SnClaimStatusCallback) {
	go connect.HandleError(func() {
		ctx, cancel := context.WithTimeout(self.ctx, snClaimStatusTimeout)
		defer cancel()
		result := self.snClaimStatus(ctx)
		if callback != nil {
			callback.Result(result, nil)
		}
	})
}

// snClaimNow asks the relay to claim one coldkey's balance now, the connected
// wallet's when none is named.
func (self *snDevice) snClaimNow(ctx context.Context, coldkeySs58 string) *SnClaimNowResult {
	return snClaimNowOf(
		coldkeySs58,
		func() (string, bool) {
			wallet := self.GetSnWallet()
			if wallet == nil {
				return "", false
			}
			return wallet.ColdkeySs58, true
		},
		func(args *SnClaimRelayClaimNowArgs) (*SnClaimRelayClaimNowResult, error) {
			return self.api().SnClaimRelayClaimNowSyncWithContext(ctx, args)
		},
	)
}

// snClaimNowOf is a Claim now tap on a device and on a page with no device
// (sn_claim_relay_js.go). connected names the connected wallet's coldkey, false
// without a wallet. post sends the tap to the server, and is not called for an
// address that is not one.
func snClaimNowOf(
	coldkeySs58 string,
	connected func() (string, bool),
	post func(*SnClaimRelayClaimNowArgs) (*SnClaimRelayClaimNowResult, error),
) *SnClaimNowResult {
	coldkeySs58 = strings.TrimSpace(coldkeySs58)
	if coldkeySs58 == "" {
		wallet, ok := connected()
		if !ok {
			return &SnClaimNowResult{Error: newSnError(SnErrorCodeConnectWalletFirst, "connect a Bittensor wallet first")}
		}
		coldkeySs58 = strings.TrimSpace(wallet)
	}
	if _, err := snColdkeyBytes(coldkeySs58); err != nil {
		return &SnClaimNowResult{Error: newSnError(SnErrorCodeInvalidAddress, err.Error())}
	}
	answer, err := post(&SnClaimRelayClaimNowArgs{ColdkeySs58: coldkeySs58})
	if err == nil && answer == nil {
		err = errors.New("the claim relay did not answer")
	}
	if err != nil {
		return &SnClaimNowResult{Error: newSnError(SnErrorCodeServer, err.Error())}
	}
	if answer.Error != nil {
		return &SnClaimNowResult{Error: newSnError(SnErrorCodeLedgerUnavailable, answer.Error.Message)}
	}
	return &SnClaimNowResult{Requested: answer.Requested, Reason: answer.Reason, NextAtMillis: answer.NextAtMillis}
}

// SnClaimNow asks URnetwork to claim a coldkey's balance now, the connected
// wallet's when coldkeySs58 is empty, as SnClaimStatus says it can.
func (self *snDevice) SnClaimNow(coldkeySs58 string, callback SnClaimNowCallback) {
	go connect.HandleError(func() {
		ctx, cancel := context.WithTimeout(self.ctx, snClaimStatusTimeout)
		defer cancel()
		result := self.snClaimNow(ctx, coldkeySs58)
		if callback != nil {
			callback.Result(result, nil)
		}
	})
}
