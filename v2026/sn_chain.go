package sdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026/sn/evm"
)

// Subtensor EVM chain ids.
const (
	SnChainIdMainnet int64 = 964
	SnChainIdTestnet int64 = 945
)

// Transaction types the claim path can sign.
const (
	SnTxTypeLegacy  = "legacy"
	SnTxTypeEip1559 = "eip1559"
)

const (
	snDefaultLookbackEpochs int64 = 128
	snRpcTimeout                  = 30 * time.Second
)

// SnChainSettings are the release constants for settling UR protocol
// entitlements: the chain, the vault and coordinator contracts, URnetwork's
// network-operator id and where payout artifacts are published. Defaults
// are compiled in; the network space values and the device's local
// settings override them (see DeviceLocal.GetSnChainSettings).
type SnChainSettings struct {
	ChainId            int64  `json:"chain_id"`
	VaultAddress       string `json:"vault_address"`
	CoordinatorAddress string `json:"coordinator_address"`
	// URnetwork's network-operator id in the coordinator (decimal or 0x hex)
	NoId   string `json:"no_id"`
	Netuid int64  `json:"netuid"`
	// "https://…/tx/%s"
	ExplorerTxUrl string `json:"explorer_tx_url"`
	// where "/sn/artifact?hash=…" is served; "" = the network space api url
	ArtifactBaseUrl string `json:"artifact_base_url"`
	// SnTxTypeLegacy (default) or SnTxTypeEip1559
	TxType string `json:"tx_type"`
	// how many epochs back SnClaims scans (default 128)
	LookbackEpochs int64 `json:"lookback_epochs"`

	rpcUrls []string
}

type snChainSettingsJson struct {
	ChainId            int64    `json:"chain_id"`
	VaultAddress       string   `json:"vault_address"`
	CoordinatorAddress string   `json:"coordinator_address"`
	NoId               string   `json:"no_id"`
	Netuid             int64    `json:"netuid"`
	ExplorerTxUrl      string   `json:"explorer_tx_url"`
	ArtifactBaseUrl    string   `json:"artifact_base_url"`
	TxType             string   `json:"tx_type"`
	LookbackEpochs     int64    `json:"lookback_epochs"`
	RpcUrls            []string `json:"rpc_urls"`
}

func NewSnChainSettings() *SnChainSettings {
	return &SnChainSettings{}
}

// DefaultSnChainSettings returns the mainnet release constants. The contract
// addresses and operator id are filled by the network space values, the
// server's /sn/epoch (DeviceLocal.SyncSnChainSettings) or the device
// settings; the chain, rpc and explorer are compiled in.
func DefaultSnChainSettings() *SnChainSettings {
	return &SnChainSettings{
		ChainId:        SnChainIdMainnet,
		Netuid:         25,
		ExplorerTxUrl:  "https://evm.taostats.io/tx/%s",
		TxType:         SnTxTypeLegacy,
		LookbackEpochs: snDefaultLookbackEpochs,
		rpcUrls:        []string{"https://lite.chain.opentensor.ai"},
	}
}

// SnTestnetChainSettings returns the Bittensor testnet constants.
func SnTestnetChainSettings() *SnChainSettings {
	return &SnChainSettings{
		ChainId:        SnChainIdTestnet,
		Netuid:         25,
		ExplorerTxUrl:  "https://evm-testnet.taostats.io/tx/%s",
		TxType:         SnTxTypeLegacy,
		LookbackEpochs: snDefaultLookbackEpochs,
		rpcUrls:        []string{"https://test.chain.opentensor.ai"},
	}
}

func (self *SnChainSettings) RpcUrls() *StringList {
	list := NewStringList()
	list.addAll(self.rpcUrls...)
	return list
}

func (self *SnChainSettings) SetRpcUrls(urls *StringList) {
	self.rpcUrls = nil
	if urls != nil {
		self.rpcUrls = append([]string(nil), urls.values...)
	}
}

func (self *SnChainSettings) AddRpcUrl(url string) {
	url = strings.TrimSpace(url)
	if url != "" {
		self.rpcUrls = append(self.rpcUrls, url)
	}
}

func (self *SnChainSettings) ClearRpcUrls() {
	self.rpcUrls = nil
}

func (self *SnChainSettings) Copy() *SnChainSettings {
	if self == nil {
		return nil
	}
	c := *self
	c.rpcUrls = append([]string(nil), self.rpcUrls...)
	return &c
}

// Merge overlays the non-empty fields of overrides on a copy of self.
func (self *SnChainSettings) Merge(overrides *SnChainSettings) *SnChainSettings {
	merged := self.Copy()
	if merged == nil {
		merged = &SnChainSettings{}
	}
	if overrides == nil {
		return merged
	}
	if overrides.ChainId != 0 {
		merged.ChainId = overrides.ChainId
	}
	if overrides.VaultAddress != "" {
		merged.VaultAddress = overrides.VaultAddress
	}
	if overrides.CoordinatorAddress != "" {
		merged.CoordinatorAddress = overrides.CoordinatorAddress
	}
	if overrides.NoId != "" {
		merged.NoId = overrides.NoId
	}
	if overrides.Netuid != 0 {
		merged.Netuid = overrides.Netuid
	}
	if overrides.ExplorerTxUrl != "" {
		merged.ExplorerTxUrl = overrides.ExplorerTxUrl
	}
	if overrides.ArtifactBaseUrl != "" {
		merged.ArtifactBaseUrl = overrides.ArtifactBaseUrl
	}
	if overrides.TxType != "" {
		merged.TxType = overrides.TxType
	}
	if overrides.LookbackEpochs != 0 {
		merged.LookbackEpochs = overrides.LookbackEpochs
	}
	if len(overrides.rpcUrls) > 0 {
		merged.rpcUrls = append([]string(nil), overrides.rpcUrls...)
	}
	return merged
}

// IsConfigured reports whether claims can be read: rpc, vault, coordinator
// and operator id are all set.
func (self *SnChainSettings) IsConfigured() bool {
	return self != nil && self.validate() == nil
}

func (self *SnChainSettings) validate() error {
	if self == nil {
		return errors.New("nil chain settings")
	}
	if len(self.rpcUrls) == 0 {
		return errors.New("no rpc urls")
	}
	if self.ChainId <= 0 {
		return errors.New("chain id not set")
	}
	if _, err := self.vault(); err != nil {
		return fmt.Errorf("vault address: %w", err)
	}
	if _, err := self.coordinator(); err != nil {
		return fmt.Errorf("coordinator address: %w", err)
	}
	if _, err := self.noId(); err != nil {
		return fmt.Errorf("no id: %w", err)
	}
	return nil
}

// ExplorerUrlForTx renders the explorer link for a transaction hash.
func (self *SnChainSettings) ExplorerUrlForTx(txHash string) string {
	if self == nil || self.ExplorerTxUrl == "" || txHash == "" {
		return ""
	}
	if strings.Contains(self.ExplorerTxUrl, "%s") {
		return fmt.Sprintf(self.ExplorerTxUrl, txHash)
	}
	return strings.TrimSuffix(self.ExplorerTxUrl, "/") + "/" + txHash
}

func (self *SnChainSettings) MarshalJSON() ([]byte, error) {
	return json.Marshal(&snChainSettingsJson{
		ChainId:            self.ChainId,
		VaultAddress:       self.VaultAddress,
		CoordinatorAddress: self.CoordinatorAddress,
		NoId:               self.NoId,
		Netuid:             self.Netuid,
		ExplorerTxUrl:      self.ExplorerTxUrl,
		ArtifactBaseUrl:    self.ArtifactBaseUrl,
		TxType:             self.TxType,
		LookbackEpochs:     self.LookbackEpochs,
		RpcUrls:            self.rpcUrls,
	})
}

func (self *SnChainSettings) UnmarshalJSON(b []byte) error {
	var j snChainSettingsJson
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*self = SnChainSettings{
		ChainId:            j.ChainId,
		VaultAddress:       j.VaultAddress,
		CoordinatorAddress: j.CoordinatorAddress,
		NoId:               j.NoId,
		Netuid:             j.Netuid,
		ExplorerTxUrl:      j.ExplorerTxUrl,
		ArtifactBaseUrl:    j.ArtifactBaseUrl,
		TxType:             j.TxType,
		LookbackEpochs:     j.LookbackEpochs,
		rpcUrls:            j.RpcUrls,
	}
	return nil
}

func (self *SnChainSettings) vault() ([20]byte, error) {
	return evm.ParseAddress(self.VaultAddress)
}

func (self *SnChainSettings) coordinator() ([20]byte, error) {
	return evm.ParseAddress(self.CoordinatorAddress)
}

func (self *SnChainSettings) noId() (*big.Int, error) {
	v, err := evm.ParseUint256(self.NoId)
	if err != nil {
		return nil, err
	}
	if v.Sign() == 0 {
		return nil, errors.New("no id is zero")
	}
	return v, nil
}

func (self *SnChainSettings) chainClient() *evm.Client {
	return evm.NewClient(self.rpcUrls, snRpcTimeout)
}

func (self *SnChainSettings) lookback() int64 {
	if self.LookbackEpochs > 0 {
		return self.LookbackEpochs
	}
	return snDefaultLookbackEpochs
}

// GetSnChainSettings returns the effective settings: compiled defaults,
// overlaid by the network space values, overlaid by the device settings
// stored with SetSnChainSettings.
func (self *snDevice) GetSnChainSettings() *SnChainSettings {
	settings := DefaultSnChainSettings()
	if self.networkSpace != nil {
		if values := self.networkSpace.values; values.SnChain != nil {
			settings = settings.Merge(values.SnChain)
		}
	}
	if localState := self.snLocalState(); localState != nil {
		settings = settings.Merge(localState.GetSnChainSettings())
	}
	return settings
}

// SetSnChainSettings stores device overrides (nil clears them).
func (self *snDevice) SetSnChainSettings(settings *SnChainSettings) error {
	localState := self.snLocalState()
	if localState == nil {
		return errors.New(SnErrorCodeLocalState)
	}
	return localState.SetSnChainSettings(settings)
}

// SyncSnChainSettings fills the contract addresses and operator id from the
// server's /sn/epoch when they are not configured yet. This is release
// configuration discovery only; claims themselves never go through the api.
func (self *snDevice) SyncSnChainSettings(callback SnEpochCallback) {
	go connect.HandleError(func() {
		result, err := self.api().SnEpochSyncWithContext(self.ctx)
		if err == nil && result != nil {
			overrides := &SnChainSettings{
				ChainId:            result.ChainId,
				CoordinatorAddress: result.ContractAddress,
				VaultAddress:       result.SettlementVaultAddress,
				Netuid:             result.Netuid,
			}
			if result.NoId != 0 {
				overrides.NoId = fmt.Sprintf("%d", result.NoId)
			}
			if result.RpcUrl != "" {
				overrides.AddRpcUrl(result.RpcUrl)
			}
			if localState := self.snLocalState(); localState != nil {
				merged := localState.GetSnChainSettings().Merge(overrides)
				// best effort: the merged settings are also returned through GetSnChainSettings
				_ = localState.SetSnChainSettings(merged)
			}
		}
		if callback != nil {
			callback.Result(result, err)
		}
	})
}
