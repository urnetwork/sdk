package sdk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026/sn/evm"
	"github.com/urnetwork/sdk/v2026/sn/merkle"
)

// Claims are direct between this device and the STSettlementVault: the
// entitlement and claimed flags are read from the vault over JSON-RPC, the
// network's leaf and Merkle proof come from the payout artifact published
// under the content hash the chain recorded (verified against the vault's
// payout root), and the claim transaction is signed here with the device gas
// key and sent to the chain endpoint. No URnetwork api is involved at claim
// time.

// SnEpochClaim.Status values.
const (
	SnClaimStatusOpen         = "open"          // epoch funded/in flight, not finalized yet
	SnClaimStatusClaimable    = "claimable"     // finalized, proof verified, unclaimed, not expired
	SnClaimStatusClaimed      = "claimed"       // leaf already claimed on chain
	SnClaimStatusExpired      = "expired"       // claim window passed (or funds carried)
	SnClaimStatusNotFinalized = "not-finalized" // no record / root missed
)

const (
	snRpcBatchSize         = 40
	snClaimReceiptPoll     = 3 * time.Second
	snClaimReceiptTimeout  = 4 * time.Minute
	snClaimsTimeout        = 2 * time.Minute
	snClaimTimeout         = 6 * time.Minute
	snArtifactMaxBytes     = 32 << 20
	snArtifactFetchTimeout = 60 * time.Second
	// artifacts are served by the URnetwork api host by convention when no
	// ArtifactBaseUrl is configured and no network space is available
	snDefaultArtifactBaseUrl = "https://api.bringyour.com"
)

// SnEpochClaim is one epoch's entitlement for the connected coldkey.
type SnEpochClaim struct {
	Epoch    int64 `json:"epoch"`
	ShareBps int64 `json:"share_bps"`
	// entitlement total × share / 10000, in rao
	AmountRao      int64  `json:"amount_rao"`
	Status         string `json:"status"`
	ClaimOpenBlock int64  `json:"claim_open_block"`
	ExpiryBlock    int64  `json:"expiry_block"`
	// last known claim tx for this epoch sent from this device, "" if none
	TxHash       string `json:"tx_hash,omitempty"`
	PayoutRoot   string `json:"payout_root,omitempty"`
	ArtifactHash string `json:"artifact_hash,omitempty"`
	// why the epoch is not claimable when the status alone does not say
	Message string `json:"message,omitempty"`
}

type SnEpochClaimList struct {
	exportedList[*SnEpochClaim]
}

func NewSnEpochClaimList() *SnEpochClaimList {
	return &SnEpochClaimList{
		exportedList: *newExportedList[*SnEpochClaim](),
	}
}

type SnClaimsResult struct {
	Claims            *SnEpochClaimList `json:"claims"`
	TotalClaimableRao int64             `json:"total_claimable_rao"`
	CurrentEpoch      int64             `json:"current_epoch"`
	BlockNumber       int64             `json:"block_number"`
	ColdkeySs58       string            `json:"coldkey_ss58,omitempty"`
	Error             *SnError          `json:"error,omitempty"`
}

type SnClaimsCallback connect.ApiCallback[*SnClaimsResult]

// SnClaimCallback receives the progress of SnClaim. Failed messages start
// with an SnErrorCode* value followed by ": detail".
type SnClaimCallback interface {
	Sent(epoch int64, txHash string)
	Confirmed(epoch int64, txHash string, amountRao int64)
	Failed(epoch int64, message string)
	Done()
}

// SnUnsignedTx is a claim transaction for an external signer.
type SnUnsignedTx struct {
	Epoch   int64  `json:"epoch"`
	ChainId int64  `json:"chain_id"`
	To      string `json:"to"`
	// 0x hex calldata
	Data string `json:"data"`
	// decimal wei, always "0"
	Value     string `json:"value"`
	AmountRao int64  `json:"amount_rao"`
}

type SnUnsignedTxList struct {
	exportedList[*SnUnsignedTx]
}

func NewSnUnsignedTxList() *SnUnsignedTxList {
	return &SnUnsignedTxList{
		exportedList: *newExportedList[*SnUnsignedTx](),
	}
}

// payout artifact (sn/payoutartifact canonical json)

type snPayoutLeaf struct {
	Index    uint64     `json:"index"`
	ClientID [16]byte   `json:"allocation_client_id"`
	Coldkey  [32]byte   `json:"coldkey"`
	ShareBPS uint64     `json:"share_bps"`
	Proof    [][32]byte `json:"proof"`
}

type snPayoutArtifact struct {
	Schema          string         `json:"schema"`
	ChainID         uint64         `json:"chain_id"`
	Netuid          uint64         `json:"netuid"`
	Coordinator     string         `json:"coordinator"`
	SettlementVault string         `json:"settlement_vault"`
	Epoch           uint64         `json:"epoch"`
	NoID            uint64         `json:"no_id"`
	Leaves          []snPayoutLeaf `json:"leaves"`
	PayoutRoot      [32]byte       `json:"payout_root"`
	SharesTotalBPS  uint64         `json:"shares_total_bps"`
	ContentHash     string         `json:"content_hash"`
}

type snArtifactFetcher func(ctx context.Context, url string) ([]byte, error)

type snArtifactStore interface {
	get(hashHex string) []byte
	put(hashHex string, raw []byte)
}

type snMemoryArtifactStore struct {
	mutex     sync.Mutex
	artifacts map[string][]byte
}

func newSnMemoryArtifactStore() *snMemoryArtifactStore {
	return &snMemoryArtifactStore{artifacts: map[string][]byte{}}
}

func (self *snMemoryArtifactStore) get(hashHex string) []byte {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.artifacts[hashHex]
}

func (self *snMemoryArtifactStore) put(hashHex string, raw []byte) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.artifacts[hashHex] = raw
}

// snLocalArtifactStore keeps verified artifacts in memory and on disk
// (content addressed, so they never go stale).
type snLocalArtifactStore struct {
	localState *LocalState
	memory     *snMemoryArtifactStore
}

func (self *snLocalArtifactStore) get(hashHex string) []byte {
	if raw := self.memory.get(hashHex); raw != nil {
		return raw
	}
	if self.localState != nil {
		if raw := self.localState.getSnArtifact(hashHex); raw != nil {
			self.memory.put(hashHex, raw)
			return raw
		}
	}
	return nil
}

func (self *snLocalArtifactStore) put(hashHex string, raw []byte) {
	self.memory.put(hashHex, raw)
	if self.localState != nil {
		_ = self.localState.setSnArtifact(hashHex, raw)
	}
}

func snHttpFetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: snArtifactFetchTimeout}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("http %d fetching the payout artifact", res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, snArtifactMaxBytes))
}

// snArtifactUnsignedBytes rebuilds the bytes the artifact content hash covers:
// the canonical json with signer, content_hash and signature zeroed
// (sn/payoutartifact.unsignedBytes), without needing the full struct.
func snArtifactUnsignedBytes(raw []byte) ([]byte, error) {
	replacements := map[string]string{
		"signer":       `"0x0000000000000000000000000000000000000000"`,
		"content_hash": `""`,
		"signature":    `""`,
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("payout artifact is not a json object")
	}
	out := make([]byte, 0, len(raw))
	last := int64(0)
	depth := 1
	expectKey := true
	replaced := 0
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("payout artifact json: %w", err)
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 1 {
					expectKey = true
				}
			}
		case string:
			if depth != 1 {
				continue
			}
			if !expectKey {
				expectKey = true
				continue
			}
			expectKey = false
			replacement, ok := replacements[t]
			if !ok {
				continue
			}
			keyEnd := dec.InputOffset()
			valueTok, err := dec.Token()
			if err != nil {
				return nil, fmt.Errorf("payout artifact json: %w", err)
			}
			if _, ok := valueTok.(string); !ok {
				return nil, fmt.Errorf("payout artifact %s is not a string", t)
			}
			valueEnd := dec.InputOffset()
			valueStart := keyEnd
			for valueStart < valueEnd {
				c := raw[valueStart]
				if c == ':' || c == ' ' || c == '\n' || c == '\r' || c == '\t' {
					valueStart++
					continue
				}
				break
			}
			out = append(out, raw[last:valueStart]...)
			out = append(out, replacement...)
			last = valueEnd
			replaced++
			expectKey = true
		default:
			if depth == 1 {
				expectKey = true
			}
		}
	}
	if replaced != len(replacements) {
		return nil, errors.New("payout artifact is missing signer, content_hash or signature")
	}
	out = append(out, raw[last:]...)
	return out, nil
}

// snParsePayoutArtifact verifies the content hash against the on-chain
// artifact hash and the artifact identity against the entitlement.
func snParsePayoutArtifact(raw []byte, wantHash [32]byte, epoch int64, noId *big.Int, payoutRoot [32]byte) (*snPayoutArtifact, error) {
	raw = bytes.TrimSpace(raw)
	unsigned, err := snArtifactUnsignedBytes(raw)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(unsigned)
	if wantHash != ([32]byte{}) && sum != wantHash {
		return nil, errors.New("payout artifact content hash does not match the chain")
	}
	var artifact snPayoutArtifact
	if err := json.Unmarshal(raw, &artifact); err != nil {
		return nil, fmt.Errorf("payout artifact: %w", err)
	}
	if !strings.EqualFold(artifact.ContentHash, "sha256:"+hex.EncodeToString(sum[:])) {
		return nil, errors.New("payout artifact content hash field does not match its bytes")
	}
	if int64(artifact.Epoch) != epoch {
		return nil, fmt.Errorf("payout artifact is for epoch %d, want %d", artifact.Epoch, epoch)
	}
	if noId != nil && new(big.Int).SetUint64(artifact.NoID).Cmp(noId) != 0 {
		return nil, errors.New("payout artifact is for another network operator")
	}
	if payoutRoot != ([32]byte{}) && artifact.PayoutRoot != payoutRoot {
		return nil, errors.New("payout artifact root does not match the vault")
	}
	return &artifact, nil
}

// engine

type snClaimPlan struct {
	epoch    int64
	shareBps *big.Int
	proof    [][32]byte
	amount   *big.Int
	claim    *SnEpochClaim
}

type snClaimEngine struct {
	settings        *SnChainSettings
	client          *evm.Client
	chainId         *big.Int
	coldkeySs58     string
	coldkey         [32]byte
	vault           [20]byte
	coordinator     [20]byte
	noId            *big.Int
	artifactBaseUrl string
	fetch           snArtifactFetcher
	store           snArtifactStore
	txHashes        map[int64]string
	onSent          func(epoch int64, txHash string)
}

func newSnClaimEngine(settings *SnChainSettings, coldkeySs58 string, artifactBaseUrl string, fetch snArtifactFetcher, store snArtifactStore, txHashes map[int64]string) (*snClaimEngine, *SnError) {
	if err := settings.validate(); err != nil {
		return nil, newSnError(SnErrorCodeChainNotConfigured, err.Error())
	}
	coldkey, err := snColdkeyBytes(coldkeySs58)
	if err != nil {
		return nil, newSnError(SnErrorCodeInvalidAddress, err.Error())
	}
	vault, _ := settings.vault()
	coordinator, _ := settings.coordinator()
	noId, _ := settings.noId()
	if artifactBaseUrl == "" {
		artifactBaseUrl = settings.ArtifactBaseUrl
	}
	if artifactBaseUrl == "" {
		artifactBaseUrl = snDefaultArtifactBaseUrl
	}
	if store == nil {
		store = newSnMemoryArtifactStore()
	}
	if fetch == nil {
		fetch = snHttpFetch
	}
	if txHashes == nil {
		txHashes = map[int64]string{}
	}
	return &snClaimEngine{
		settings:        settings,
		client:          settings.chainClient(),
		chainId:         big.NewInt(settings.ChainId),
		coldkeySs58:     strings.TrimSpace(coldkeySs58),
		coldkey:         coldkey,
		vault:           vault,
		coordinator:     coordinator,
		noId:            noId,
		artifactBaseUrl: strings.TrimSuffix(artifactBaseUrl, "/"),
		fetch:           fetch,
		store:           store,
		txHashes:        txHashes,
	}, nil
}

func (self *snClaimEngine) currentEpoch(ctx context.Context) (int64, error) {
	ret, err := self.client.EthCall(ctx, self.coordinator, evm.PackCurrentEpoch())
	if err != nil {
		return 0, err
	}
	v, err := evm.DecodeUint(ret)
	if err != nil {
		return 0, err
	}
	if !v.IsInt64() {
		return 0, errors.New("current epoch overflows int64")
	}
	return v.Int64(), nil
}

func (self *snClaimEngine) entitlements(ctx context.Context, epochs []int64) (map[int64]*evm.Entitlement, error) {
	out := map[int64]*evm.Entitlement{}
	for start := 0; start < len(epochs); start += snRpcBatchSize {
		end := min(start+snRpcBatchSize, len(epochs))
		datas := make([][]byte, 0, end-start)
		for _, epoch := range epochs[start:end] {
			data, err := evm.PackEntitlement(big.NewInt(epoch), self.noId)
			if err != nil {
				return nil, err
			}
			datas = append(datas, data)
		}
		rets, errs, err := self.client.EthCallBatch(ctx, self.vault, datas)
		if err != nil {
			return nil, err
		}
		for i, epoch := range epochs[start:end] {
			if errs[i] != nil {
				return nil, errs[i]
			}
			ent, err := evm.DecodeEntitlement(rets[i])
			if err != nil {
				return nil, err
			}
			out[epoch] = ent
		}
	}
	return out, nil
}

func (self *snClaimEngine) claimedFlags(ctx context.Context, epochs []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	key, err := evm.ClaimKey(self.noId, self.coldkey)
	if err != nil {
		return nil, err
	}
	for start := 0; start < len(epochs); start += snRpcBatchSize {
		end := min(start+snRpcBatchSize, len(epochs))
		datas := make([][]byte, 0, end-start)
		for _, epoch := range epochs[start:end] {
			data, err := evm.PackLeafClaimed(big.NewInt(epoch), key)
			if err != nil {
				return nil, err
			}
			datas = append(datas, data)
		}
		rets, errs, err := self.client.EthCallBatch(ctx, self.vault, datas)
		if err != nil {
			return nil, err
		}
		for i, epoch := range epochs[start:end] {
			if errs[i] != nil {
				return nil, errs[i]
			}
			claimed, err := evm.DecodeBool(rets[i])
			if err != nil {
				return nil, err
			}
			out[epoch] = claimed
		}
	}
	return out, nil
}

func (self *snClaimEngine) artifactUrl(hash [32]byte) string {
	return fmt.Sprintf("%s/sn/artifact?hash=sha256:%s", self.artifactBaseUrl, hex.EncodeToString(hash[:]))
}

func (self *snClaimEngine) artifact(ctx context.Context, hash [32]byte, epoch int64, payoutRoot [32]byte) (*snPayoutArtifact, error) {
	if hash == ([32]byte{}) {
		return nil, errors.New("the chain has no artifact hash for this epoch yet")
	}
	hashHex := hex.EncodeToString(hash[:])
	if raw := self.store.get(hashHex); raw != nil {
		if artifact, err := snParsePayoutArtifact(raw, hash, epoch, self.noId, payoutRoot); err == nil {
			return artifact, nil
		}
	}
	fetchCtx, cancel := context.WithTimeout(ctx, snArtifactFetchTimeout)
	defer cancel()
	raw, err := self.fetch(fetchCtx, self.artifactUrl(hash))
	if err != nil {
		return nil, err
	}
	artifact, err := snParsePayoutArtifact(raw, hash, epoch, self.noId, payoutRoot)
	if err != nil {
		return nil, err
	}
	self.store.put(hashHex, bytes.TrimSpace(raw))
	return artifact, nil
}

func (self *snClaimEngine) leafFor(artifact *snPayoutArtifact) *snPayoutLeaf {
	for i := range artifact.Leaves {
		if artifact.Leaves[i].Coldkey == self.coldkey {
			return &artifact.Leaves[i]
		}
	}
	return nil
}

// views resolves the claim rows and plans for the epochs. block is the
// current chain block; currentEpoch the coordinator's current epoch.
func (self *snClaimEngine) views(ctx context.Context, epochs []int64, block uint64, currentEpoch int64) ([]*SnEpochClaim, map[int64]*snClaimPlan, error) {
	ents, err := self.entitlements(ctx, epochs)
	if err != nil {
		return nil, nil, err
	}
	var finalized []int64
	for _, epoch := range epochs {
		if ent := ents[epoch]; ent != nil && ent.Status == evm.StatusFinalized {
			finalized = append(finalized, epoch)
		}
	}
	claimed := map[int64]bool{}
	if len(finalized) > 0 {
		claimed, err = self.claimedFlags(ctx, finalized)
		if err != nil {
			return nil, nil, err
		}
	}
	claims := []*SnEpochClaim{}
	plans := map[int64]*snClaimPlan{}
	for _, epoch := range epochs {
		ent := ents[epoch]
		if ent == nil {
			continue
		}
		txHash := self.txHashes[epoch]
		recent := epoch >= currentEpoch-2
		claim := &SnEpochClaim{
			Epoch:       epoch,
			ExpiryBlock: int64(ent.ExpiryBlock),
			TxHash:      txHash,
		}
		if ent.PayoutRoot != ([32]byte{}) {
			claim.PayoutRoot = "0x" + hex.EncodeToString(ent.PayoutRoot[:])
		}
		if ent.ArtifactHash != ([32]byte{}) {
			claim.ArtifactHash = "sha256:" + hex.EncodeToString(ent.ArtifactHash[:])
		}
		switch ent.Status {
		case evm.StatusFinalized:
			artifact, err := self.artifact(ctx, ent.ArtifactHash, epoch, ent.PayoutRoot)
			if err != nil {
				if !recent && txHash == "" {
					continue
				}
				claim.Status = SnClaimStatusOpen
				claim.Message = SnErrorCodeArtifactUnavailable + ": " + err.Error()
				claims = append(claims, claim)
				continue
			}
			leaf := self.leafFor(artifact)
			if leaf == nil {
				// no share for this coldkey in this epoch
				if txHash == "" {
					continue
				}
				claim.Status = SnClaimStatusNotFinalized
				claim.Message = "no share in this epoch"
				claims = append(claims, claim)
				continue
			}
			shareBps := new(big.Int).SetUint64(leaf.ShareBPS)
			claim.ShareBps = int64(leaf.ShareBPS)
			if !merkle.Verify(ent.PayoutRoot, merkle.PayoutLeaf(self.coldkey, shareBps), leaf.Proof) {
				claim.Status = SnClaimStatusOpen
				claim.Message = SnErrorCodeProofMismatch + ": the published proof does not verify against the vault root"
				claims = append(claims, claim)
				continue
			}
			amount := new(big.Int).Mul(ent.Total, shareBps)
			amount.Div(amount, big.NewInt(evm.BPS))
			if amount.IsInt64() {
				claim.AmountRao = amount.Int64()
			}
			switch {
			case claimed[epoch]:
				claim.Status = SnClaimStatusClaimed
			case block > ent.ExpiryBlock:
				claim.Status = SnClaimStatusExpired
			default:
				claim.Status = SnClaimStatusClaimable
				plans[epoch] = &snClaimPlan{
					epoch:    epoch,
					shareBps: shareBps,
					proof:    leaf.Proof,
					amount:   amount,
					claim:    claim,
				}
			}
			claims = append(claims, claim)
		case evm.StatusFunded:
			claim.Status = SnClaimStatusOpen
			claims = append(claims, claim)
		case evm.StatusUnset:
			if epoch < currentEpoch-1 && txHash == "" {
				continue
			}
			claim.Status = SnClaimStatusOpen
			claims = append(claims, claim)
		case evm.StatusRootMissed:
			if !recent && txHash == "" {
				continue
			}
			claim.Status = SnClaimStatusNotFinalized
			claim.Message = "payout root missed"
			claims = append(claims, claim)
		case evm.StatusCarried:
			if !recent && txHash == "" {
				continue
			}
			claim.Status = SnClaimStatusExpired
			claim.Message = "unclaimed funds carried forward"
			claims = append(claims, claim)
		default:
			continue
		}
	}
	return claims, plans, nil
}

func (self *snClaimEngine) chainHead(ctx context.Context) (uint64, int64, error) {
	block, err := self.client.BlockNumber(ctx)
	if err != nil {
		return 0, 0, err
	}
	currentEpoch, err := self.currentEpoch(ctx)
	if err != nil {
		return 0, 0, err
	}
	return block, currentEpoch, nil
}

// claims scans from max(fromEpoch, current - lookback) to the current epoch.
func (self *snClaimEngine) claims(ctx context.Context, fromEpoch int64) *SnClaimsResult {
	result := &SnClaimsResult{Claims: NewSnEpochClaimList(), ColdkeySs58: self.coldkeySs58}
	block, currentEpoch, err := self.chainHead(ctx)
	if err != nil {
		result.Error = snErrorFromErr(err)
		return result
	}
	result.BlockNumber = int64(block)
	result.CurrentEpoch = currentEpoch
	start := max(currentEpoch-self.settings.lookback(), 0)
	if fromEpoch > start {
		start = fromEpoch
	}
	epochs := make([]int64, 0, max(currentEpoch-start+1, 0))
	for epoch := start; epoch <= currentEpoch; epoch++ {
		epochs = append(epochs, epoch)
	}
	claims, plans, err := self.views(ctx, epochs, block, currentEpoch)
	if err != nil {
		result.Error = snErrorFromErr(err)
		return result
	}
	// newest first
	for i := len(claims) - 1; i >= 0; i-- {
		result.Claims.Add(claims[i])
	}
	var total int64
	for _, plan := range plans {
		if plan.amount.IsInt64() {
			total += plan.amount.Int64()
		}
	}
	result.TotalClaimableRao = total
	return result
}

func (self *snClaimEngine) calldata(plan *snClaimPlan) ([]byte, error) {
	return evm.PackClaim(big.NewInt(plan.epoch), self.noId, self.coldkey, plan.shareBps, plan.proof)
}

// plan resolves a single epoch; a nil plan comes with the reason.
func (self *snClaimEngine) plan(ctx context.Context, epoch int64) (*snClaimPlan, string, error) {
	block, currentEpoch, err := self.chainHead(ctx)
	if err != nil {
		return nil, "", err
	}
	claims, plans, err := self.views(ctx, []int64{epoch}, block, currentEpoch)
	if err != nil {
		return nil, "", err
	}
	if plan := plans[epoch]; plan != nil {
		return plan, "", nil
	}
	for _, claim := range claims {
		switch claim.Status {
		case SnClaimStatusClaimed:
			return nil, SnErrorCodeAlreadyClaimed + ": epoch already claimed", nil
		case SnClaimStatusExpired:
			return nil, SnErrorCodeExpired + ": the claim window for this epoch has closed", nil
		default:
			reason := claim.Status
			if claim.Message != "" {
				reason = claim.Message
			}
			return nil, SnErrorCodeNotClaimable + ": " + reason, nil
		}
	}
	return nil, SnErrorCodeNotClaimable + ": no entitlement for this coldkey in this epoch", nil
}

func (self *snClaimEngine) unsignedTxs(ctx context.Context, epochs []int64) (*SnUnsignedTxList, error) {
	list := NewSnUnsignedTxList()
	var firstReason string
	for _, epoch := range epochs {
		plan, reason, err := self.plan(ctx, epoch)
		if err != nil {
			return nil, err
		}
		if plan == nil {
			if firstReason == "" {
				firstReason = fmt.Sprintf("epoch %d: %s", epoch, reason)
			}
			continue
		}
		data, err := self.calldata(plan)
		if err != nil {
			return nil, err
		}
		tx := &SnUnsignedTx{
			Epoch:   epoch,
			ChainId: self.settings.ChainId,
			To:      evm.AddressHex(self.vault),
			Data:    "0x" + hex.EncodeToString(data),
			Value:   "0",
		}
		if plan.amount.IsInt64() {
			tx.AmountRao = plan.amount.Int64()
		}
		list.Add(tx)
	}
	if list.Len() == 0 {
		if firstReason == "" {
			firstReason = "no epochs"
		}
		return nil, errors.New(firstReason)
	}
	return list, nil
}

func snFailMessage(err error) string {
	var rpcErr *evm.RpcError
	switch {
	case errors.Is(err, evm.ErrUnreachable):
		return SnErrorCodeChainRpcUnreachable + ": " + err.Error()
	case errors.As(err, &rpcErr):
		reason := rpcErr.Reason()
		lower := strings.ToLower(reason)
		switch {
		case strings.Contains(lower, "insufficient funds") || strings.Contains(lower, "balance too low"):
			return SnErrorCodeNeedsGas + ": " + reason
		case reason == "AlreadyClaimed":
			return SnErrorCodeAlreadyClaimed + ": " + reason
		case reason == "ClaimExpired":
			return SnErrorCodeExpired + ": " + reason
		case reason == "InvalidProof":
			return SnErrorCodeProofMismatch + ": " + reason
		default:
			return SnErrorCodeClaimFailed + ": " + reason
		}
	default:
		return SnErrorCodeClaimFailed + ": " + err.Error()
	}
}

func snFormatTao(wei *big.Int) string {
	tao, _ := new(big.Float).Quo(new(big.Float).SetInt(wei), snWeiPerTao).Float64()
	return fmt.Sprintf("%.6f TAO", tao)
}

// send claims one epoch with the gas key and reports through the callback.
func (self *snClaimEngine) send(ctx context.Context, key *secp.PrivateKey, epoch int64, callback SnClaimCallback) {
	plan, reason, err := self.plan(ctx, epoch)
	if err != nil {
		callback.Failed(epoch, snFailMessage(err))
		return
	}
	if plan == nil {
		callback.Failed(epoch, reason)
		return
	}
	data, err := self.calldata(plan)
	if err != nil {
		callback.Failed(epoch, SnErrorCodeClaimFailed+": "+err.Error())
		return
	}
	from := evm.AddressOf(key)
	gasKey := snGasKeyInfo(key)
	balance, err := self.client.Balance(ctx, from)
	if err != nil {
		callback.Failed(epoch, snFailMessage(err))
		return
	}
	if balance.Sign() == 0 {
		callback.Failed(epoch, fmt.Sprintf("%s: the gas key %s has no TAO; send a little TAO to %s", SnErrorCodeNeedsGas, gasKey.Address, gasKey.MirrorSs58))
		return
	}
	gas, err := self.client.EstimateGas(ctx, from, self.vault, data, nil)
	if err != nil {
		callback.Failed(epoch, snFailMessage(err))
		return
	}
	gas += gas / 4

	var raw []byte
	var maxCost *big.Int
	useDynamic := false
	var baseFee *big.Int
	if self.settings.TxType == SnTxTypeEip1559 {
		if fee, err := self.client.LatestBaseFee(ctx); err == nil && fee != nil && fee.Sign() > 0 {
			baseFee = fee
			useDynamic = true
		}
	}
	nonce, err := self.client.Nonce(ctx, from)
	if err != nil {
		callback.Failed(epoch, snFailMessage(err))
		return
	}
	if useDynamic {
		tip, err := self.client.MaxPriorityFeePerGas(ctx)
		if err != nil || tip == nil {
			tip = big.NewInt(1_000_000_000)
		}
		feeCap := new(big.Int).Mul(baseFee, big.NewInt(2))
		feeCap.Add(feeCap, tip)
		maxCost = new(big.Int).Mul(feeCap, new(big.Int).SetUint64(gas))
		tx := &evm.DynamicFeeTx{Nonce: nonce, MaxPriorityFeePerGas: tip, MaxFeePerGas: feeCap, Gas: gas, To: self.vault, Value: big.NewInt(0), Data: data}
		raw, _, err = tx.Signed(self.chainId, key)
		if err != nil {
			callback.Failed(epoch, SnErrorCodeClaimFailed+": "+err.Error())
			return
		}
	} else {
		gasPrice, err := self.client.GasPrice(ctx)
		if err != nil {
			callback.Failed(epoch, snFailMessage(err))
			return
		}
		gasPrice = new(big.Int).Div(new(big.Int).Mul(gasPrice, big.NewInt(11)), big.NewInt(10))
		if gasPrice.Sign() == 0 {
			gasPrice = big.NewInt(1)
		}
		maxCost = new(big.Int).Mul(gasPrice, new(big.Int).SetUint64(gas))
		tx := &evm.LegacyTx{Nonce: nonce, GasPrice: gasPrice, Gas: gas, To: self.vault, Value: big.NewInt(0), Data: data}
		raw, _, err = tx.Signed(self.chainId, key)
		if err != nil {
			callback.Failed(epoch, SnErrorCodeClaimFailed+": "+err.Error())
			return
		}
	}
	if balance.Cmp(maxCost) < 0 {
		callback.Failed(epoch, fmt.Sprintf("%s: this claim needs up to %s of gas, the gas key %s has %s; send TAO to %s", SnErrorCodeNeedsGas, snFormatTao(maxCost), gasKey.Address, snFormatTao(balance), gasKey.MirrorSs58))
		return
	}
	txHash, err := self.client.SendRawTransaction(ctx, raw)
	if err != nil {
		callback.Failed(epoch, snFailMessage(err))
		return
	}
	self.txHashes[epoch] = txHash
	if self.onSent != nil {
		self.onSent(epoch, txHash)
	}
	callback.Sent(epoch, txHash)
	receipt, err := self.client.WaitReceipt(ctx, txHash, snClaimReceiptPoll, snClaimReceiptTimeout)
	if err != nil {
		callback.Failed(epoch, SnErrorCodeClaimFailed+": "+err.Error())
		return
	}
	if !receipt.Status {
		callback.Failed(epoch, SnErrorCodeClaimFailed+": the claim transaction reverted")
		return
	}
	amountRao := int64(0)
	if plan.amount.IsInt64() {
		amountRao = plan.amount.Int64()
	}
	callback.Confirmed(epoch, txHash, amountRao)
}

// device

func (self *snDevice) snClaimEngine() (*snClaimEngine, *SnError) {
	wallet := self.GetSnWallet()
	if wallet == nil {
		return nil, newSnError(SnErrorCodeConnectWalletFirst, "connect a Bittensor wallet first")
	}
	settings := self.GetSnChainSettings()
	artifactBaseUrl := settings.ArtifactBaseUrl
	if artifactBaseUrl == "" && self.networkSpace != nil {
		artifactBaseUrl = self.networkSpace.GetApiUrl()
	}
	localState := self.snLocalState()
	var txHashes map[int64]string
	if localState != nil {
		txHashes = localState.getSnClaimTxHashes()
	}
	api := self.api()
	fetch := func(ctx context.Context, url string) ([]byte, error) {
		return api.getHttpGetRaw()(ctx, url, "")
	}
	store := &snLocalArtifactStore{localState: localState, memory: self.state.artifacts}
	engine, snErr := newSnClaimEngine(settings, wallet.ColdkeySs58, artifactBaseUrl, fetch, store, txHashes)
	if snErr != nil {
		return nil, snErr
	}
	engine.onSent = func(epoch int64, txHash string) {
		if localState != nil {
			_ = localState.setSnClaimTxHash(epoch, txHash)
		}
	}
	return engine, nil
}

func (self *snDevice) snWalletFromEpoch() int64 {
	if wallet := self.GetSnWallet(); wallet != nil {
		return wallet.FromEpoch
	}
	return 0
}

// SnClaims reads the connected coldkey's entitlements from the vault.
func (self *snDevice) SnClaims(callback SnClaimsCallback) {
	go connect.HandleError(func() {
		var result *SnClaimsResult
		engine, snErr := self.snClaimEngine()
		if snErr != nil {
			result = &SnClaimsResult{Claims: NewSnEpochClaimList(), Error: snErr}
		} else {
			ctx, cancel := context.WithTimeout(self.ctx, snClaimsTimeout)
			defer cancel()
			result = engine.claims(ctx, self.snWalletFromEpoch())
		}
		if callback != nil {
			callback.Result(result, nil)
		}
	})
}

// SnClaim sends claim transactions for the epochs, one after another,
// signed with the device gas key.
func (self *snDevice) SnClaim(epochs *Int64List, callback SnClaimCallback) {
	go connect.HandleError(func() {
		defer callback.Done()
		var requested []int64
		if epochs != nil {
			requested = append(requested, epochs.values...)
		}
		engine, snErr := self.snClaimEngine()
		if snErr != nil {
			for _, epoch := range requested {
				callback.Failed(epoch, snErr.Code+": "+snErr.Message)
			}
			return
		}
		key, err := self.snGasPrivateKey()
		if err != nil {
			for _, epoch := range requested {
				callback.Failed(epoch, SnErrorCodeLocalState+": "+err.Error())
			}
			return
		}
		for _, epoch := range requested {
			ctx, cancel := context.WithTimeout(self.ctx, snClaimTimeout)
			engine.send(ctx, key, epoch, callback)
			cancel()
		}
	})
}

// SnClaimTransactions builds unsigned claim transactions for an external
// signer.
func (self *snDevice) SnClaimTransactions(epochs *Int64List) (*SnUnsignedTxList, error) {
	engine, snErr := self.snClaimEngine()
	if snErr != nil {
		return nil, errors.New(snErr.Code + ": " + snErr.Message)
	}
	ctx, cancel := context.WithTimeout(self.ctx, snClaimsTimeout)
	defer cancel()
	var requested []int64
	if epochs != nil {
		requested = append(requested, epochs.values...)
	}
	return engine.unsignedTxs(ctx, requested)
}

// hosts without a DeviceLocal (web)

// SnClaimsFor reads a coldkey's entitlements with explicit settings.
// fromEpoch = 0 scans the full lookback window.
func SnClaimsFor(settings *SnChainSettings, coldkeySs58 string, fromEpoch int64, callback SnClaimsCallback) {
	go connect.HandleError(func() {
		var result *SnClaimsResult
		engine, snErr := newSnClaimEngine(settings, coldkeySs58, "", nil, nil, nil)
		if snErr != nil {
			result = &SnClaimsResult{Claims: NewSnEpochClaimList(), Error: snErr}
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), snClaimsTimeout)
			defer cancel()
			result = engine.claims(ctx, fromEpoch)
		}
		if callback != nil {
			callback.Result(result, nil)
		}
	})
}

// SnClaimTransactionsFor builds unsigned claim transactions with explicit
// settings, for an injected browser wallet.
func SnClaimTransactionsFor(settings *SnChainSettings, coldkeySs58 string, epochs *Int64List) (*SnUnsignedTxList, error) {
	engine, snErr := newSnClaimEngine(settings, coldkeySs58, "", nil, nil, nil)
	if snErr != nil {
		return nil, errors.New(snErr.Code + ": " + snErr.Message)
	}
	ctx, cancel := context.WithTimeout(context.Background(), snClaimsTimeout)
	defer cancel()
	var requested []int64
	if epochs != nil {
		requested = append(requested, epochs.values...)
	}
	return engine.unsignedTxs(ctx, requested)
}
