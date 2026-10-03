package sdk

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/urnetwork/connect"
)

// The Bittensor coldkey where the network's UR protocol entitlements settle.
// A wallet is an account setting kept by the server (POST/GET /sn/wallet)
// and cached on the device in local state so the earnings screens can gate
// the subnet layer without a round trip.
type SnWallet struct {
	ColdkeySs58 string `json:"coldkey_ss58"`
	// the provider client this wallet is attached to; "" = network-level
	ClientId    string `json:"client_id,omitempty"`
	SetAtMillis int64  `json:"set_at_millis"`
	// first epoch the wallet is effective for (0 = unknown); alpha is never
	// retroactive, so claims are scanned from here
	FromEpoch int64 `json:"from_epoch,omitempty"`
}

func (self *SnWallet) Copy() *SnWallet {
	if self == nil {
		return nil
	}
	c := *self
	return &c
}

type SnWalletList struct {
	exportedList[*SnWallet]
}

func NewSnWalletList() *SnWalletList {
	return &SnWalletList{
		exportedList: *newExportedList[*SnWallet](),
	}
}

// Api

type SnSetWalletCallback connect.ApiCallback[*SnSetWalletResult]

// SnSetWallet is the async twin of SnSetWalletSync (POST /sn/wallet).
func (self *Api) SnSetWallet(args *SnSetWalletArgs, callback SnSetWalletCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/sn/wallet", self.apiUrl),
			args,
			self.GetByJwt(),
			&SnSetWalletResult{},
			callback,
		)
	})
}

type SnGetWalletResult struct {
	// the network-level wallet
	Wallet *SnWallet `json:"wallet,omitempty"`
	// every wallet on the account (network-level and per client)
	Wallets *SnWalletList `json:"wallets,omitempty"`
	Error   *SnError      `json:"error,omitempty"`
}

type SnGetWalletCallback connect.ApiCallback[*SnGetWalletResult]

// SnGetWallet reads the caller's wallets (GET /sn/wallet).
func (self *Api) SnGetWallet(callback SnGetWalletCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/sn/wallet", self.apiUrl),
			self.GetByJwt(),
			&SnGetWalletResult{},
			callback,
		)
	})
}

//gomobile:noexport
func (self *Api) SnGetWalletSyncWithContext(ctx context.Context) (*SnGetWalletResult, error) {
	return connect.HttpGetWithRawFunction(
		ctx,
		self.getHttpGetRaw(),
		fmt.Sprintf("%s/sn/wallet", self.apiUrl),
		self.GetByJwt(),
		&SnGetWalletResult{},
		connect.NewNoopApiCallback[*SnGetWalletResult](),
	)
}

type SnValidateWalletArgs struct {
	Address string `json:"address"`
}

// SnValidateWalletResult is the server's verdict on a coldkey address
// (POST /sn/wallet/validate, unauthenticated). ValidSyntax mirrors the
// local ValidateSs58 check; ExistsOnChain=false is a warning, Banned=true
// blocks the address from being sent anywhere.
type SnValidateWalletResult struct {
	ValidSyntax   bool     `json:"valid_syntax"`
	ExistsOnChain bool     `json:"exists_on_chain"`
	Banned        bool     `json:"banned"`
	Message       string   `json:"message,omitempty"`
	Error         *SnError `json:"error,omitempty"`
}

type SnValidateWalletCallback connect.ApiCallback[*SnValidateWalletResult]

// SnValidateWallet asks the server about an address without authentication.
func (self *Api) SnValidateWallet(address string, callback SnValidateWalletCallback) {
	go connect.HandleError(func() {
		self.snValidateWallet(self.ctx, address, callback)
	})
}

func (self *Api) snValidateWallet(ctx context.Context, address string, callback SnValidateWalletCallback) (*SnValidateWalletResult, error) {
	return connect.HttpPostWithRawFunction(
		ctx,
		self.getHttpPostRaw(),
		fmt.Sprintf("%s/sn/wallet/validate", self.apiUrl),
		&SnValidateWalletArgs{Address: strings.TrimSpace(address)},
		"",
		&SnValidateWalletResult{},
		callback,
	)
}

//gomobile:noexport
func (self *Api) SnValidateWalletSyncWithContext(ctx context.Context, address string) (*SnValidateWalletResult, error) {
	return self.snValidateWallet(ctx, address, connect.NewNoopApiCallback[*SnValidateWalletResult]())
}

// Device

type SnWalletChangeListener interface {
	SnWalletChanged(wallet *SnWallet)
}

// SnConnectWalletResult reports a ConnectSnWallet outcome. ExistsOnChain is
// informational: a false value with a nil Error means the wallet was
// attached but the coldkey has never been seen on chain (warn, continue).
type SnConnectWalletResult struct {
	Wallet        *SnWallet `json:"wallet,omitempty"`
	ExistsOnChain bool      `json:"exists_on_chain"`
	// "" or the localization key of a non-blocking warning
	Warning string   `json:"warning,omitempty"`
	Error   *SnError `json:"error,omitempty"`
}

type SnConnectWalletCallback connect.ApiCallback[*SnConnectWalletResult]

// deviceSn holds the device-side UR protocol state.
type deviceSn struct {
	mutex                 sync.Mutex
	wallet                *SnWallet
	walletLoaded          bool
	walletChangeListeners *connect.CallbackList[SnWalletChangeListener]
	gasKey                *secp.PrivateKey
	artifacts             *snMemoryArtifactStore
}

func newDeviceSn() *deviceSn {
	return &deviceSn{
		walletChangeListeners: connect.NewCallbackList[SnWalletChangeListener](),
		artifacts:             newSnMemoryArtifactStore(),
	}
}

// snDevice is the view both DeviceLocal and DeviceRemote hand to the UR
// protocol methods: everything they need lives in the process that calls
// them (local state, the api, the client id) except the client key, which
// only the DeviceLocal holds (DeviceRemote forwards signing over rpc).
type snDevice struct {
	ctx           context.Context
	networkSpace  *NetworkSpace
	api           func() *Api
	clientId      connect.Id
	state         *deviceSn
	clientKeySeed func() []byte
}

func (self *snDevice) snLocalState() *LocalState {
	if self.networkSpace == nil {
		return nil
	}
	if asyncLocalState := self.networkSpace.GetAsyncLocalState(); asyncLocalState != nil {
		return asyncLocalState.GetLocalState()
	}
	return nil
}

// GetSnWallet returns the cached wallet, or nil when none is connected.
func (self *snDevice) GetSnWallet() *SnWallet {
	self.state.mutex.Lock()
	defer self.state.mutex.Unlock()
	if !self.state.walletLoaded {
		self.state.walletLoaded = true
		if localState := self.snLocalState(); localState != nil {
			self.state.wallet = localState.GetSnWallet()
		}
	}
	return self.state.wallet.Copy()
}

func (self *snDevice) setSnWallet(wallet *SnWallet) {
	self.state.mutex.Lock()
	self.state.wallet = wallet.Copy()
	self.state.walletLoaded = true
	self.state.mutex.Unlock()
	if localState := self.snLocalState(); localState != nil {
		// best effort; the in-memory cache is authoritative for this session
		_ = localState.SetSnWallet(wallet)
	}
	self.snWalletChanged(wallet.Copy())
}

// ClearSnWalletCache forgets the cached wallet on this device only.
func (self *snDevice) ClearSnWalletCache() {
	self.setSnWallet(nil)
}

func (self *snDevice) AddSnWalletChangeListener(listener SnWalletChangeListener) Sub {
	callbackId := self.state.walletChangeListeners.Add(listener)
	return newSub(func() {
		self.state.walletChangeListeners.Remove(callbackId)
	})
}

func (self *snDevice) snWalletChanged(wallet *SnWallet) {
	for _, listener := range self.state.walletChangeListeners.Get() {
		connect.HandleError(func() {
			listener.SnWalletChanged(wallet)
		})
	}
}

// ConnectSnWallet attaches a coldkey to this device's client: local syntax
// check, the server's validate endpoint (a banned address is never sent
// further), then the signed POST /sn/wallet, then cache + notify.
func (self *snDevice) ConnectSnWallet(coldkeySs58 string, signature string, message string, callback SnConnectWalletCallback) {
	go connect.HandleError(func() {
		ctx, cancel := context.WithTimeout(self.ctx, 60*time.Second)
		defer cancel()
		result, err := self.connectSnWallet(ctx, coldkeySs58, signature, message)
		if callback != nil {
			callback.Result(result, err)
		}
	})
}

func (self *snDevice) connectSnWallet(ctx context.Context, coldkeySs58 string, signature string, message string) (*SnConnectWalletResult, error) {
	address := strings.TrimSpace(coldkeySs58)
	if !ValidateSs58(address) {
		return &SnConnectWalletResult{Error: newSnError(SnErrorCodeInvalidAddress, "not a valid Bittensor address")}, nil
	}
	api := self.api()
	validation, err := api.SnValidateWalletSyncWithContext(ctx, address)
	if err != nil {
		return nil, err
	}
	if validation.Error != nil {
		return &SnConnectWalletResult{Error: validation.Error}, nil
	}
	if !validation.ValidSyntax {
		return &SnConnectWalletResult{Error: newSnError(SnErrorCodeInvalidAddress, validation.Message)}, nil
	}
	if validation.Banned {
		return &SnConnectWalletResult{Error: newSnError(SnErrorCodeWalletBlocked, validation.Message)}, nil
	}
	set, err := api.SnSetWalletSyncWithContext(ctx, &SnSetWalletArgs{
		ColdkeySs58: address,
		ClientId:    newId(self.clientId),
		Signature:   signature,
		Message:     message,
	})
	if err != nil {
		return nil, err
	}
	if set.Error != nil {
		return &SnConnectWalletResult{Error: newSnError(SnErrorCodeServer, set.Error.Message)}, nil
	}
	wallet := set.Wallet.Copy()
	if wallet == nil {
		wallet = &SnWallet{}
	}
	if wallet.ColdkeySs58 == "" {
		wallet.ColdkeySs58 = address
	}
	if wallet.ClientId == "" {
		wallet.ClientId = newId(self.clientId).String()
	}
	if wallet.SetAtMillis == 0 {
		wallet.SetAtMillis = time.Now().UnixMilli()
	}
	self.setSnWallet(wallet)
	result := &SnConnectWalletResult{Wallet: wallet.Copy(), ExistsOnChain: validation.ExistsOnChain}
	if !validation.ExistsOnChain {
		result.Warning = "wallet_looks_new_warning"
	}
	return result, nil
}

// SyncSnWallet refreshes the cache from the server (GET /sn/wallet), for
// example after signing in on a new device.
func (self *snDevice) SyncSnWallet(callback SnGetWalletCallback) {
	go connect.HandleError(func() {
		ctx, cancel := context.WithTimeout(self.ctx, 60*time.Second)
		defer cancel()
		result, err := self.api().SnGetWalletSyncWithContext(ctx)
		if err == nil && result != nil && result.Error == nil {
			wallet := snPickWallet(result, newId(self.clientId).String())
			current := self.GetSnWallet()
			if wallet == nil {
				if current != nil {
					self.setSnWallet(nil)
				}
			} else if current == nil || *current != *wallet {
				self.setSnWallet(wallet)
			}
		}
		if callback != nil {
			callback.Result(result, err)
		}
	})
}

// snPickWallet prefers the wallet attached to this client, then the
// network-level wallet, then the first one.
func snPickWallet(result *SnGetWalletResult, clientId string) *SnWallet {
	var networkLevel *SnWallet
	var first *SnWallet
	if result.Wallets != nil {
		for _, wallet := range result.Wallets.values {
			if wallet == nil || wallet.ColdkeySs58 == "" {
				continue
			}
			if wallet.ClientId == clientId {
				return wallet.Copy()
			}
			if wallet.ClientId == "" && networkLevel == nil {
				networkLevel = wallet
			}
			if first == nil {
				first = wallet
			}
		}
	}
	if result.Wallet != nil && result.Wallet.ColdkeySs58 != "" {
		if result.Wallet.ClientId == clientId {
			return result.Wallet.Copy()
		}
		if networkLevel == nil && result.Wallet.ClientId == "" {
			networkLevel = result.Wallet
		}
		if first == nil {
			first = result.Wallet
		}
	}
	if networkLevel != nil {
		return networkLevel.Copy()
	}
	return first.Copy()
}
