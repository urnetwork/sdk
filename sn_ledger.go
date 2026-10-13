package sdk

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk/sn/merkle"
)

// From its switch epoch an operator pool settles in STSettlementVault2
// ("vault2"): each payout coldkey has one account per pool, a cumulative
// balance committed in the pool's ledger roots that never expires. The
// operator serves the account at GET /sn/ledger/account, on the artifact
// host. This reads it for display; the v1 entitlements are still read from
// the v1 vault by SnClaims and shown separately. The v2 claim
// (sn_ledger_claim.go) reads the same answer and trusts none of it.

const snLedgerTimeout = 60 * time.Second

// SnLedgerAccount is the coldkey's v2 account in one operator pool. Amounts
// are rao.
type SnLedgerAccount struct {
	// the pool's vault2, "0x" and 40 lowercase hex digits
	Vault string `json:"vault"`
	NoId  int64  `json:"no_id"`
	// the account's fixed position in the pool's tree
	Index int64 `json:"index"`
	// the cumulative in the latest finalized root; 0 while the account is
	// only in the pending root
	FinalizedRao int64 `json:"finalized_rao"`
	// finalized less paid: what a claim would transfer now
	ClaimableRao int64 `json:"claimable_rao"`
	// vault2's claimed watermark, already transferred to the coldkey
	PaidRao int64 `json:"paid_rao"`
	// a committed root that is not finalized yet holds the account, with
	// PendingRao as its cumulative (0 when false)
	Pending    bool  `json:"pending"`
	PendingRao int64 `json:"pending_rao"`
}

type SnLedgerAccountList struct {
	exportedList[*SnLedgerAccount]
}

func NewSnLedgerAccountList() *SnLedgerAccountList {
	return &SnLedgerAccountList{
		exportedList: *newExportedList[*SnLedgerAccount](),
	}
}

// SnLedgerAccountsResult lists the coldkey's v2 accounts, one per pool; an
// empty list means the ledger has no account for the coldkey. With an Error
// the balances are unavailable, never zero: SnErrorCodeLedgerUnavailable when
// the ledger cannot be read or its answer is refused.
type SnLedgerAccountsResult struct {
	Accounts    *SnLedgerAccountList `json:"accounts"`
	ColdkeySs58 string               `json:"coldkey_ss58,omitempty"`
	Error       *SnError             `json:"error,omitempty"`
}

type SnLedgerAccountsCallback connect.ApiCallback[*SnLedgerAccountsResult]

// GET /sn/ledger/account. Amounts are decimal strings of rao.
type snLedgerAccountsJson struct {
	// "0x" and 64 lowercase hex digits
	Coldkey string `json:"coldkey"`
	// null decodes to nil, [] to empty
	Accounts []snLedgerAccountJson `json:"accounts"`
}

type snLedgerAccountJson struct {
	Vault string `json:"vault"`
	NoId  int64  `json:"no_id"`
	Index int64  `json:"index"`
	// the latest finalized root's index in vault2's roots for the pool; null
	// when the account is only in the pending root, which comes with a zero
	// cumulative and a null proof
	RootIndex  *int64 `json:"root_index"`
	Cumulative string `json:"cumulative"`
	// bottom-up siblings, [] at depth 0
	Proof   []snLedgerProofNodeJson `json:"proof"`
	Claimed string                  `json:"claimed"`
	// the cumulative in the pending root; null without one
	PendingCumulative *string `json:"pending_cumulative"`
}

type snLedgerProofNodeJson struct {
	Hash string `json:"hash"`
	Sum  string `json:"sum"`
}

// snLedgerHex decodes "0x" and exactly 2×size lowercase hex digits.
func snLedgerHex(s string, size int) ([]byte, error) {
	digits, ok := strings.CutPrefix(s, "0x")
	if !ok || len(digits) != 2*size || strings.ToLower(digits) != digits {
		return nil, fmt.Errorf("%q is not 0x and %d lowercase hex digits", s, 2*size)
	}
	return hex.DecodeString(digits)
}

// snLedgerRao parses a canonical decimal amount of rao.
func snLedgerRao(s string) (int64, error) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 || strconv.FormatInt(v, 10) != s {
		return 0, fmt.Errorf("%q is not a decimal amount of rao", s)
	}
	return v, nil
}

// snLedgerEntry is one account of the answer: what the apps show, and the
// finalized root's index and proof a claim needs.
type snLedgerEntry struct {
	account *SnLedgerAccount
	// -1 when the account is only in the pending root
	rootIndex int64
	// bottom-up siblings; nil when the account is only in the pending root
	proof []merkle.LedgerNode
}

// entry checks one account and derives the states the apps show.
func (self *snLedgerAccountJson) entry() (*snLedgerEntry, error) {
	if _, err := snLedgerHex(self.Vault, 20); err != nil {
		return nil, fmt.Errorf("vault: %w", err)
	}
	if self.NoId <= 0 || self.Index < 0 {
		return nil, fmt.Errorf("no_id %d or index %d out of range", self.NoId, self.Index)
	}
	cumulative, err := snLedgerRao(self.Cumulative)
	if err != nil {
		return nil, fmt.Errorf("cumulative: %w", err)
	}
	claimed, err := snLedgerRao(self.Claimed)
	if err != nil {
		return nil, fmt.Errorf("claimed: %w", err)
	}
	var proof []merkle.LedgerNode
	if self.Proof != nil {
		proof = make([]merkle.LedgerNode, len(self.Proof))
	}
	for i, node := range self.Proof {
		hash, err := snLedgerHex(node.Hash, 32)
		if err != nil {
			return nil, fmt.Errorf("proof %d hash: %w", i, err)
		}
		sum, err := snLedgerRao(node.Sum)
		if err != nil {
			return nil, fmt.Errorf("proof %d sum: %w", i, err)
		}
		proof[i] = merkle.LedgerNode{Hash: [32]byte(hash), Sum: big.NewInt(sum)}
	}
	switch {
	case self.RootIndex == nil && (cumulative != 0 || self.Proof != nil || self.PendingCumulative == nil):
		return nil, errors.New("an account in no finalized root must be pending, with a zero cumulative and no proof")
	case self.RootIndex != nil && (*self.RootIndex < 0 || self.Proof == nil):
		return nil, errors.New("an account in a finalized root needs its root index and proof")
	case cumulative < claimed:
		return nil, fmt.Errorf("claimed %d exceeds the finalized cumulative %d", claimed, cumulative)
	}
	account := &SnLedgerAccount{
		Vault:        self.Vault,
		NoId:         self.NoId,
		Index:        self.Index,
		FinalizedRao: cumulative,
		ClaimableRao: cumulative - claimed,
		PaidRao:      claimed,
	}
	if self.PendingCumulative != nil {
		pending, err := snLedgerRao(*self.PendingCumulative)
		if err != nil {
			return nil, fmt.Errorf("pending_cumulative: %w", err)
		}
		account.Pending = true
		account.PendingRao = pending
	}
	entry := &snLedgerEntry{account: account, rootIndex: -1, proof: proof}
	if self.RootIndex != nil {
		entry.rootIndex = *self.RootIndex
	}
	return entry, nil
}

// snParseLedgerAccounts refuses the whole answer when any part of it does
// not parse exactly, so none of its balances is shown.
func snParseLedgerAccounts(raw []byte, coldkey [32]byte) ([]*snLedgerEntry, error) {
	var answer snLedgerAccountsJson
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, err
	}
	answerColdkey, err := snLedgerHex(answer.Coldkey, 32)
	if err != nil {
		return nil, fmt.Errorf("coldkey: %w", err)
	}
	if !bytes.Equal(answerColdkey, coldkey[:]) {
		return nil, errors.New("the answer is for another coldkey")
	}
	if answer.Accounts == nil {
		return nil, errors.New("the answer has no accounts list")
	}
	entries := make([]*snLedgerEntry, 0, len(answer.Accounts))
	for i := range answer.Accounts {
		entry, err := answer.Accounts[i].entry()
		if err != nil {
			return nil, fmt.Errorf("account %d: %w", i, err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// snReadLedger reads a coldkey's v2 accounts from the artifact host at
// baseUrl ("" for the default host).
func snReadLedger(ctx context.Context, fetch snArtifactFetcher, baseUrl string, coldkeySs58 string) ([]*snLedgerEntry, *SnError) {
	coldkey, err := snColdkeyBytes(coldkeySs58)
	if err != nil {
		return nil, newSnError(SnErrorCodeInvalidAddress, err.Error())
	}
	if baseUrl == "" {
		baseUrl = snDefaultArtifactBaseUrl
	}
	raw, err := fetch(ctx, fmt.Sprintf("%s/sn/ledger/account?coldkey=%s", strings.TrimSuffix(baseUrl, "/"), url.QueryEscape(coldkeySs58)))
	if err != nil {
		return nil, newSnError(SnErrorCodeLedgerUnavailable, err.Error())
	}
	entries, err := snParseLedgerAccounts(raw, coldkey)
	if err != nil {
		return nil, newSnError(SnErrorCodeLedgerUnavailable, "refused the ledger answer: "+err.Error())
	}
	return entries, nil
}

// snLedgerAccounts reads a coldkey's v2 accounts for display.
func snLedgerAccounts(ctx context.Context, fetch snArtifactFetcher, baseUrl string, coldkeySs58 string) *SnLedgerAccountsResult {
	coldkeySs58 = strings.TrimSpace(coldkeySs58)
	result := &SnLedgerAccountsResult{Accounts: NewSnLedgerAccountList(), ColdkeySs58: coldkeySs58}
	entries, snErr := snReadLedger(ctx, fetch, baseUrl, coldkeySs58)
	if snErr != nil {
		result.Error = snErr
		return result
	}
	for _, entry := range entries {
		result.Accounts.Add(entry.account)
	}
	return result
}

// SnLedgerAccounts reads the connected coldkey's v2 accounts.
func (self *snDevice) SnLedgerAccounts(callback SnLedgerAccountsCallback) {
	go connect.HandleError(func() {
		var result *SnLedgerAccountsResult
		if wallet := self.GetSnWallet(); wallet == nil {
			result = &SnLedgerAccountsResult{Accounts: NewSnLedgerAccountList(), Error: newSnError(SnErrorCodeConnectWalletFirst, "connect a Bittensor wallet first")}
		} else {
			api := self.api()
			fetch := func(ctx context.Context, requestUrl string) ([]byte, error) {
				return api.getHttpGetRaw()(ctx, requestUrl, "")
			}
			ctx, cancel := context.WithTimeout(self.ctx, snLedgerTimeout)
			defer cancel()
			result = snLedgerAccounts(ctx, fetch, self.snArtifactBaseUrl(self.GetSnChainSettings()), wallet.ColdkeySs58)
		}
		if callback != nil {
			callback.Result(result, nil)
		}
	})
}

// SnLedgerAccountsFor reads a coldkey's v2 accounts from the artifact host in
// settings (hosts without a DeviceLocal).
func SnLedgerAccountsFor(settings *SnChainSettings, coldkeySs58 string, callback SnLedgerAccountsCallback) {
	go connect.HandleError(func() {
		baseUrl := ""
		if settings != nil {
			baseUrl = settings.ArtifactBaseUrl
		}
		fetch := func(ctx context.Context, requestUrl string) ([]byte, error) {
			return snHttpGet(ctx, requestUrl, "the ledger account")
		}
		ctx, cancel := context.WithTimeout(context.Background(), snLedgerTimeout)
		defer cancel()
		result := snLedgerAccounts(ctx, fetch, baseUrl, coldkeySs58)
		if callback != nil {
			callback.Result(result, nil)
		}
	})
}
