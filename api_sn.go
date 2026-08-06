package sdk

// Go/headless SDK bindings for the subnet miner and validator control-plane
// routes. Trail-step POSTs deliberately remain in sn/validator because they
// must egress through a specifically pinned provider tunnel.

import (
	"context"
	"fmt"

	"github.com/urnetwork/connect"
)

type GetClientKeyArgs struct {
	ClientId *Id `json:"client_id"`
}

type GetClientKeyResult struct {
	PublicKey []byte `json:"public_key"`
}

//gomobile:noexport
func (self *Api) GetClientKeySyncWithContext(ctx context.Context, args *GetClientKeyArgs) (*GetClientKeyResult, error) {
	if args == nil || args.ClientId == nil {
		return nil, fmt.Errorf("client id is required")
	}
	return connect.HttpGetWithRawFunction(
		ctx,
		self.getHttpGetRaw(),
		fmt.Sprintf("%s/key/%s", self.apiUrl, args.ClientId),
		"",
		&GetClientKeyResult{},
		connect.NewNoopApiCallback[*GetClientKeyResult](),
	)
}

type VerifyServerKey struct {
	// int32, NOT byte. Go's `byte` is an alias for uint8, and gomobile's objc
	// generator emits the literal name "byte" — which is not a type in
	// Objective-C ("unknown type name 'byte'; did you mean 'Byte'?"), so
	// build_apple fails to compile the generated Sdk_darwin.m. Java has a
	// `byte`, so build_android is unaffected and the break is apple-only.
	// The value is a small key id; int32 is wire-identical in json.
	ServerKeyId int32  `json:"server_key_id"`
	PublicKey   []byte `json:"public_key"`
}

// VerifyServerKeyList is the bound form of []*VerifyServerKey.
type VerifyServerKeyList struct {
	exportedList[*VerifyServerKey]
}

func NewVerifyServerKeyList() *VerifyServerKeyList {
	return &VerifyServerKeyList{
		exportedList: *newExportedList[*VerifyServerKey](),
	}
}

// VerifyKeysResult is the set of active verify server keys.
type VerifyKeysResult struct {
	// The slice itself cannot be bound (gomobile binds neither slices of
	// struct pointers nor slices of slices), and it must keep this exact Go
	// type: []byte marshals to a base64 json string, so retyping would change
	// what the server sees. Apps reach the keys through GetKeys() below.
	//gomobile:noexport []*VerifyServerKey — bound via GetKeys()
	Keys []*VerifyServerKey `json:"keys"`
}

// GetKeys is the app-facing accessor for Keys. Without it the bound class is
// an empty shell — its only field is dropped by gobind.
func (self *VerifyKeysResult) GetKeys() *VerifyServerKeyList {
	list := NewVerifyServerKeyList()
	list.addAll(self.Keys...)
	return list
}

//gomobile:noexport
func (self *Api) VerifyKeysSyncWithContext(ctx context.Context) (*VerifyKeysResult, error) {
	return connect.HttpGetWithRawFunction(
		ctx,
		self.getHttpGetRaw(),
		fmt.Sprintf("%s/verify/keys", self.apiUrl),
		"",
		&VerifyKeysResult{},
		connect.NewNoopApiCallback[*VerifyKeysResult](),
	)
}

//gomobile:noexport
func (self *Api) VerifyKeysSync() (*VerifyKeysResult, error) {
	return self.VerifyKeysSyncWithContext(self.ctx)
}

type SnSetWalletArgs struct {
	ColdkeySs58 string `json:"coldkey_ss58"`
}

type SnSetWalletError struct {
	Message string `json:"message"`
}

type SnSetWalletResult struct {
	Error *SnSetWalletError `json:"error,omitempty"`
}

//gomobile:noexport
func (self *Api) SnSetWalletSyncWithContext(ctx context.Context, args *SnSetWalletArgs) (*SnSetWalletResult, error) {
	return connect.HttpPostWithRawFunction(
		ctx,
		self.getHttpPostRaw(),
		fmt.Sprintf("%s/sn/wallet", self.apiUrl),
		args,
		self.GetByJwt(),
		&SnSetWalletResult{},
		connect.NewNoopApiCallback[*SnSetWalletResult](),
	)
}

//gomobile:noexport
func (self *Api) SnSetWalletSync(args *SnSetWalletArgs) (*SnSetWalletResult, error) {
	return self.SnSetWalletSyncWithContext(self.ctx, args)
}

// SnPoolClaimArgs selects the epoch to claim.
//
// int64, not uint64: gomobile cannot bind uint64, and as uint64 this class
// bound as an empty shell that could not express a claim at all. An epoch
// number is nowhere near 2^63 and json is a bare number either way, so the
// wire format is unchanged.
type SnPoolClaimArgs struct {
	Epoch int64 `json:"epoch"`
}

// SnPoolClaimResult is a merkle claim against the payout pool.
//
// The epoch/chain/block numbers are int64 rather than uint64 so they bind:
// gomobile cannot bind uint64, and without them an app could not submit a
// claim from the bound class at all. All are far below 2^63 and json carries
// a bare number either way, so the wire format is unchanged.
type SnPoolClaimResult struct {
	Epoch    int64  `json:"epoch"`
	NoId     []byte `json:"no_id"`
	Coldkey  []byte `json:"coldkey"`
	ShareBps int    `json:"share_bps"`
	// Must stay [][]byte: each element marshals to a base64 json string, so
	// retyping would change the wire format. Bound via GetProof() below.
	//gomobile:noexport [][]byte — bound via GetProof()
	Proof           [][]byte `json:"proof"`
	PayoutRoot      []byte   `json:"payout_root"`
	ContractAddress string   `json:"contract_address"`
	ChainId         int64    `json:"chain_id"`
	ClaimOpenBlock  int64    `json:"claim_open_block"`
}

// GetProofLen and GetProofAt are the app-facing accessors for the merkle
// proof. gomobile binds []byte but not a slice of them; without these an app
// has the claim but not the proof that authorizes it. Each element is a
// sibling hash, in order from leaf to root.
//
// A count/at pair rather than an exportedList: that wrapper needs a bindable
// element TYPE, and a bare []byte is not one — wrapping would mean inventing
// a public ByteArray type solely to carry it. Both methods bind directly.
func (self *SnPoolClaimResult) GetProofLen() int32 {
	return int32(len(self.Proof))
}

// GetProofAt returns the branch at i, or nil when i is out of range (rather
// than panicking across the language boundary, where a Go panic is not
// recoverable by the caller).
func (self *SnPoolClaimResult) GetProofAt(i int32) []byte {
	if i < 0 || int(i) >= len(self.Proof) {
		return nil
	}
	return self.Proof[i]
}

//gomobile:noexport
func (self *Api) SnPoolClaimSyncWithContext(ctx context.Context, args *SnPoolClaimArgs) (*SnPoolClaimResult, error) {
	if args == nil {
		return nil, fmt.Errorf("pool claim args are required")
	}
	return connect.HttpGetWithRawFunction(
		ctx,
		self.getHttpGetRaw(),
		fmt.Sprintf("%s/sn/pool/claim?epoch=%d", self.apiUrl, args.Epoch),
		self.GetByJwt(),
		&SnPoolClaimResult{},
		connect.NewNoopApiCallback[*SnPoolClaimResult](),
	)
}

//gomobile:noexport
func (self *Api) SnPoolClaimSync(args *SnPoolClaimArgs) (*SnPoolClaimResult, error) {
	return self.SnPoolClaimSyncWithContext(self.ctx, args)
}

// SnEpochResult is the current epoch schedule in chain blocks.
//
// int64 throughout rather than uint64: gomobile cannot bind uint64, and as
// uint64 this class shipped with ContractAddress as its only usable field —
// the schedule itself was invisible to apps. Block heights, epoch numbers and
// chain ids are all far below 2^63, and json carries a bare number either
// way, so the wire format is unchanged.
type SnEpochResult struct {
	Epoch               int64  `json:"epoch"`
	StartBlock          int64  `json:"start_block"`
	CommitDeadlineBlock int64  `json:"commit_deadline_block"`
	TrailsDeadlineBlock int64  `json:"trails_deadline_block"`
	FinalizeBlock       int64  `json:"finalize_block"`
	TEpochBlocks        int64  `json:"t_epoch_blocks"`
	ChainId             int64  `json:"chain_id"`
	ContractAddress     string `json:"contract_address"`
}

//gomobile:noexport
func (self *Api) SnEpochSyncWithContext(ctx context.Context) (*SnEpochResult, error) {
	return connect.HttpGetWithRawFunction(
		ctx,
		self.getHttpGetRaw(),
		fmt.Sprintf("%s/sn/epoch", self.apiUrl),
		self.GetByJwt(),
		&SnEpochResult{},
		connect.NewNoopApiCallback[*SnEpochResult](),
	)
}

//gomobile:noexport
func (self *Api) SnEpochSync() (*SnEpochResult, error) {
	return self.SnEpochSyncWithContext(self.ctx)
}
