package sdk

import (
	"errors"
)

// DeviceLocal and DeviceRemote expose the same UR protocol surface. The
// wallet, gas key, vault reads and claim sends run in the calling process
// (they need local state, the api and the chain, not the tunnel), so the
// app-side DeviceRemote works even while the extension is down. Only the
// fleet-binding signature needs the client key held by the DeviceLocal;
// DeviceRemote forwards that over rpc.

func (self *DeviceLocal) snDevice() *snDevice {
	return &snDevice{
		ctx:           self.ctx,
		networkSpace:  self.networkSpace,
		api:           self.GetApi,
		clientId:      self.clientId,
		state:         self.sn,
		clientKeySeed: self.GetClientKeySeed,
	}
}

// wallet

func (self *DeviceLocal) GetSnWallet() *SnWallet {
	return self.snDevice().GetSnWallet()
}

func (self *DeviceLocal) ClearSnWalletCache() {
	self.snDevice().ClearSnWalletCache()
}

func (self *DeviceLocal) AddSnWalletChangeListener(listener SnWalletChangeListener) Sub {
	return self.snDevice().AddSnWalletChangeListener(listener)
}

func (self *DeviceLocal) ConnectSnWallet(coldkeySs58 string, signature string, message string, callback SnConnectWalletCallback) {
	self.snDevice().ConnectSnWallet(coldkeySs58, signature, message, callback)
}

func (self *DeviceLocal) SyncSnWallet(callback SnGetWalletCallback) {
	self.snDevice().SyncSnWallet(callback)
}

// gas key

func (self *DeviceLocal) GetSnGasKey() *SnGasKey {
	return self.snDevice().GetSnGasKey()
}

func (self *DeviceLocal) SnGasBalance(callback SnGasBalanceCallback) {
	self.snDevice().SnGasBalance(callback)
}

// chain settings

func (self *DeviceLocal) GetSnChainSettings() *SnChainSettings {
	return self.snDevice().GetSnChainSettings()
}

func (self *DeviceLocal) SetSnChainSettings(settings *SnChainSettings) error {
	return self.snDevice().SetSnChainSettings(settings)
}

func (self *DeviceLocal) SyncSnChainSettings(callback SnEpochCallback) {
	self.snDevice().SyncSnChainSettings(callback)
}

// claims

func (self *DeviceLocal) SnClaims(callback SnClaimsCallback) {
	self.snDevice().SnClaims(callback)
}

func (self *DeviceLocal) SnClaim(epochs *Int64List, callback SnClaimCallback) {
	self.snDevice().SnClaim(epochs, callback)
}

func (self *DeviceLocal) SnClaimTransactions(epochs *Int64List) (*SnUnsignedTxList, error) {
	return self.snDevice().SnClaimTransactions(epochs)
}

// head spot

func (self *DeviceLocal) GetSnClientKey() string {
	return self.snDevice().GetSnClientKey()
}

func (self *DeviceLocal) SignSnFleetBinding(bindingJson string) (string, error) {
	return self.snDevice().SignSnFleetBinding(bindingJson)
}

// DeviceRemote

func (self *DeviceRemote) snDevice() *snDevice {
	return &snDevice{
		ctx:          self.ctx,
		networkSpace: self.networkSpace,
		api:          self.GetApi,
		clientId:     self.clientId,
		state:        self.sn,
		// the client key lives in the DeviceLocal; see SignSnFleetBinding
		clientKeySeed: nil,
	}
}

func (self *DeviceRemote) GetSnWallet() *SnWallet {
	return self.snDevice().GetSnWallet()
}

func (self *DeviceRemote) ClearSnWalletCache() {
	self.snDevice().ClearSnWalletCache()
}

func (self *DeviceRemote) AddSnWalletChangeListener(listener SnWalletChangeListener) Sub {
	return self.snDevice().AddSnWalletChangeListener(listener)
}

func (self *DeviceRemote) ConnectSnWallet(coldkeySs58 string, signature string, message string, callback SnConnectWalletCallback) {
	self.snDevice().ConnectSnWallet(coldkeySs58, signature, message, callback)
}

func (self *DeviceRemote) SyncSnWallet(callback SnGetWalletCallback) {
	self.snDevice().SyncSnWallet(callback)
}

func (self *DeviceRemote) GetSnGasKey() *SnGasKey {
	return self.snDevice().GetSnGasKey()
}

func (self *DeviceRemote) SnGasBalance(callback SnGasBalanceCallback) {
	self.snDevice().SnGasBalance(callback)
}

func (self *DeviceRemote) GetSnChainSettings() *SnChainSettings {
	return self.snDevice().GetSnChainSettings()
}

func (self *DeviceRemote) SetSnChainSettings(settings *SnChainSettings) error {
	return self.snDevice().SetSnChainSettings(settings)
}

func (self *DeviceRemote) SyncSnChainSettings(callback SnEpochCallback) {
	self.snDevice().SyncSnChainSettings(callback)
}

func (self *DeviceRemote) SnClaims(callback SnClaimsCallback) {
	self.snDevice().SnClaims(callback)
}

func (self *DeviceRemote) SnClaim(epochs *Int64List, callback SnClaimCallback) {
	self.snDevice().SnClaim(epochs, callback)
}

func (self *DeviceRemote) SnClaimTransactions(epochs *Int64List) (*SnUnsignedTxList, error) {
	return self.snDevice().SnClaimTransactions(epochs)
}

// GetSnClientKey asks the DeviceLocal for the client public key; "" when
// the extension service is not connected.
func (self *DeviceRemote) GetSnClientKey() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.service == nil {
		return ""
	}
	key, err := rpcCallNoArg[string](self.service, "DeviceLocalRpc.GetSnClientKey", self.closeService)
	if err != nil {
		return ""
	}
	return key
}

// SignSnFleetBinding forwards to the DeviceLocal, which holds the client
// key. Fails when the extension service is not connected.
func (self *DeviceRemote) SignSnFleetBinding(bindingJson string) (string, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.service == nil {
		return "", errors.New("device service not connected")
	}
	signature, err := rpcCall[string](self.service, "DeviceLocalRpc.SignSnFleetBinding", bindingJson, self.closeService)
	if err != nil {
		return "", err
	}
	if signature == "" {
		return "", errors.New("the device could not sign the binding")
	}
	return signature, nil
}

// DeviceLocalRpc

func (self *DeviceLocalRpc) GetSnClientKey(_ RpcNoArg, key *string) error {
	*key = self.deviceLocal.GetSnClientKey()
	return nil
}

func (self *DeviceLocalRpc) SignSnFleetBinding(bindingJson string, signature *string) error {
	signed, err := self.deviceLocal.SignSnFleetBinding(bindingJson)
	if err != nil {
		return err
	}
	*signature = signed
	return nil
}
