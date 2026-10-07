package sdk

// Go/headless SDK bindings for the subnet miner and validator control-plane
// routes. Trail-step POSTs deliberately remain in sn/validator because they
// must egress through a specifically pinned provider tunnel.

import (
	"context"
	"fmt"
	"net/url"

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

// Selects the provider and explicit earning interval for a wallet consent.
// Signed int64 epochs preserve mobile bindings and serialize as json numbers.
type SnWalletMappingChallengeArgs struct {
	ClientId     *Id    `json:"client_id,omitempty"`
	ColdkeySs58  string `json:"coldkey_ss58"`
	FromEpoch    int64  `json:"from_epoch"`
	ThroughEpoch int64  `json:"through_epoch"`
}

// The caller signs these exact server-issued bytes after verifying their domain.
type SnWalletMappingChallengeResult struct {
	Message string `json:"message"`
}

// Requests a consent challenge through the api's authenticated post transport.
//
//gomobile:noexport
func (self *Api) SnWalletMappingChallengeSyncWithContext(ctx context.Context, args *SnWalletMappingChallengeArgs) (*SnWalletMappingChallengeResult, error) {
	if args == nil {
		return nil, fmt.Errorf("wallet mapping challenge args are required")
	}
	if args.FromEpoch < 0 || args.ThroughEpoch < 0 || args.ThroughEpoch < args.FromEpoch {
		return nil, fmt.Errorf("wallet mapping epoch interval must be nonnegative and ordered")
	}
	return connect.HttpPostWithRawFunction(
		ctx,
		self.getHttpPostRaw(),
		fmt.Sprintf("%s/sn/wallet/consent", self.apiUrl),
		args,
		self.GetByJwt(),
		&SnWalletMappingChallengeResult{},
		connect.NewNoopApiCallback[*SnWalletMappingChallengeResult](),
	)
}

// Uses the api lifetime for the challenge request.
//
//gomobile:noexport
func (self *Api) SnWalletMappingChallengeSync(args *SnWalletMappingChallengeArgs) (*SnWalletMappingChallengeResult, error) {
	return self.SnWalletMappingChallengeSyncWithContext(self.ctx, args)
}

// Selects the explicit earning interval of a network wallet consent: the
// coldkey signs once for every provider client of the session's network. Only
// the network owner's session (a network JWT) may request it; submit the
// signed message through SnSetWallet without a client id.
type SnNetworkWalletMappingChallengeArgs struct {
	ColdkeySs58  string `json:"coldkey_ss58"`
	FromEpoch    int64  `json:"from_epoch"`
	ThroughEpoch int64  `json:"through_epoch"`
}

// Requests a network consent challenge through the api's authenticated post
// transport.
//
//gomobile:noexport
func (self *Api) SnNetworkWalletMappingChallengeSyncWithContext(ctx context.Context, args *SnNetworkWalletMappingChallengeArgs) (*SnWalletMappingChallengeResult, error) {
	if args == nil {
		return nil, fmt.Errorf("network wallet mapping challenge args are required")
	}
	if args.FromEpoch < 0 || args.ThroughEpoch < 0 || args.ThroughEpoch < args.FromEpoch {
		return nil, fmt.Errorf("wallet mapping epoch interval must be nonnegative and ordered")
	}
	return connect.HttpPostWithRawFunction(
		ctx,
		self.getHttpPostRaw(),
		fmt.Sprintf("%s/sn/wallet/network-consent", self.apiUrl),
		args,
		self.GetByJwt(),
		&SnWalletMappingChallengeResult{},
		connect.NewNoopApiCallback[*SnWalletMappingChallengeResult](),
	)
}

// Uses the api lifetime for the network challenge request.
//
//gomobile:noexport
func (self *Api) SnNetworkWalletMappingChallengeSync(args *SnNetworkWalletMappingChallengeArgs) (*SnWalletMappingChallengeResult, error) {
	return self.SnNetworkWalletMappingChallengeSyncWithContext(self.ctx, args)
}

type SnSetWalletArgs struct {
	ColdkeySs58 string `json:"coldkey_ss58"`
	ClientId    *Id    `json:"client_id,omitempty"`
	// Coldkey sr25519 signature (hex) over the exact consent or wallet-login
	// challenge message issued by the server.
	Signature string `json:"signature,omitempty"`
	Message   string `json:"message,omitempty"`
}

type SnSetWalletError struct {
	// the server's stable code for the refusal, when it has one
	// (SnErrorCodeSignatureMismatch); "" from older servers and for other
	// refusals
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

type SnSetWalletResult struct {
	// the stored wallet (with its first effective epoch) when the server returns it
	Wallet *SnWallet         `json:"wallet,omitempty"`
	Error  *SnSetWalletError `json:"error,omitempty"`
	// Accepted consent identity for callers verifying the mapping publication.
	MappingHash       string `json:"mapping_hash,omitempty"`
	MappingGeneration int64  `json:"mapping_generation,omitempty"`
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

// Selects the epoch and, optionally, its original legacy proof coldkey.
//
// int64, not uint64: gomobile cannot bind uint64, and as uint64 this class
// bound as an empty shell that could not express a claim at all. An epoch
// number is nowhere near 2^63 and json is a bare number either way, so the
// wire format is unchanged.
type SnPoolClaimArgs struct {
	Epoch int64 `json:"epoch"`
	// Original coldkey ss58 for a legacy network-only epoch's proof. This
	// selector does not confer ownership or authorization.
	LegacyColdkey string `json:"legacy_coldkey,omitempty"`
}

type SnPoolClaimError struct {
	Message string `json:"message"`
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
	Proof                  [][]byte          `json:"proof"`
	PayoutRoot             []byte            `json:"payout_root"`
	ContractAddress        string            `json:"contract_address"`
	ChainId                int64             `json:"chain_id"`
	ClaimOpenBlock         int64             `json:"claim_open_block"`
	ArtifactHash           string            `json:"artifact_hash,omitempty"`
	ArtifactUri            string            `json:"artifact_uri,omitempty"`
	SettlementVaultAddress string            `json:"settlement_vault_address,omitempty"`
	Error                  *SnPoolClaimError `json:"error,omitempty"`
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
	requestUrl := fmt.Sprintf("%s/sn/pool/claim?epoch=%d", self.apiUrl, args.Epoch)
	if args.LegacyColdkey != "" {
		requestUrl += "&legacy_coldkey=" + url.QueryEscape(args.LegacyColdkey)
	}
	return connect.HttpGetWithRawFunction(
		ctx,
		self.getHttpGetRaw(),
		requestUrl,
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
// chain ids are all far below 2^63. Numeric json encoding is retained; no_id
// also accepts the server's decimal-string encoding when decoded.
//
// The genesis hash is the subnet chain's genesis block hash, "0x" and 64
// lowercase hex digits; with the chain id and netuid it names the subnet. It
// is empty from a server that predates it.
type SnEpochResult struct {
	Epoch               int64  `json:"epoch"`
	StartBlock          int64  `json:"start_block"`
	CommitDeadlineBlock int64  `json:"commit_deadline_block"`
	TrailsDeadlineBlock int64  `json:"trails_deadline_block"`
	FinalizeBlock       int64  `json:"finalize_block"`
	TEpochBlocks        int64  `json:"t_epoch_blocks"`
	ChainId             int64  `json:"chain_id"`
	GenesisHash         string `json:"genesis_hash,omitempty"`
	ContractAddress     string `json:"contract_address"`
	// optional release configuration for the direct claim path
	SettlementVaultAddress string `json:"settlement_vault_address,omitempty"`
	NoId                   int64  `json:"no_id,omitempty"`
	Netuid                 int64  `json:"netuid,omitempty"`
	RpcUrl                 string `json:"rpc_url,omitempty"`
}

type SnEpochCallback connect.ApiCallback[*SnEpochResult]

func (self *Api) SnEpoch(callback SnEpochCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/sn/epoch", self.apiUrl),
			self.GetByJwt(),
			&SnEpochResult{},
			callback,
		)
	})
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
