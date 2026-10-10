package sdk

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Local state for the UR protocol layer. Everything is per network space,
// next to the other local state files. The gas key secret never leaves this
// file and this process.
const (
	snWalletFile   = ".sn_wallet"
	snGasKeyFile   = ".sn_gas_key"
	snChainFile    = ".sn_chain"
	snClaimTxFile  = ".sn_claim_tx"
	snArtifactDir  = ".sn_artifacts"
	snArtifactMode = 0700
)

var snHexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (self *LocalState) snPath(name string) string {
	return filepath.Join(self.localStorageDir, name)
}

func (self *LocalState) snReadJson(name string, out any) bool {
	b, err := os.ReadFile(self.snPath(name))
	if err != nil {
		return false
	}
	return json.Unmarshal(b, out) == nil
}

func (self *LocalState) snWriteJson(name string, value any) error {
	if value == nil {
		err := os.Remove(self.snPath(name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(self.snPath(name), b, LocalStorageFilePermissions)
}

// GetSnWallet returns the cached wallet or nil.
func (self *LocalState) GetSnWallet() *SnWallet {
	var wallet SnWallet
	if !self.snReadJson(snWalletFile, &wallet) || wallet.ColdkeySs58 == "" {
		return nil
	}
	return &wallet
}

// SetSnWallet caches the wallet (nil clears it).
func (self *LocalState) SetSnWallet(wallet *SnWallet) error {
	if wallet == nil {
		return self.snWriteJson(snWalletFile, nil)
	}
	return self.snWriteJson(snWalletFile, wallet)
}

// GetSnChainSettings returns the device overrides or nil.
func (self *LocalState) GetSnChainSettings() *SnChainSettings {
	var settings SnChainSettings
	if !self.snReadJson(snChainFile, &settings) {
		return nil
	}
	return &settings
}

// SetSnChainSettings stores the device overrides (nil clears them).
func (self *LocalState) SetSnChainSettings(settings *SnChainSettings) error {
	if settings == nil {
		return self.snWriteJson(snChainFile, nil)
	}
	return self.snWriteJson(snChainFile, settings)
}

func (self *LocalState) getSnGasKeySecret() []byte {
	b, err := os.ReadFile(self.snPath(snGasKeyFile))
	if err != nil {
		return nil
	}
	secret, err := hex.DecodeString(strings.TrimSpace(strings.TrimPrefix(string(b), "0x")))
	if err != nil || len(secret) != 32 {
		return nil
	}
	return secret
}

func (self *LocalState) setSnGasKeySecret(secret []byte) error {
	if len(secret) != 32 {
		return errors.New("gas key secret must be 32 bytes")
	}
	return os.WriteFile(self.snPath(snGasKeyFile), []byte(hex.EncodeToString(secret)), LocalStorageFilePermissions)
}

func (self *LocalState) getSnClaimTxHashes() map[int64]string {
	raw := map[string]string{}
	self.snReadJson(snClaimTxFile, &raw)
	out := map[int64]string{}
	for k, v := range raw {
		if epoch, err := strconv.ParseInt(k, 10, 64); err == nil && v != "" {
			out[epoch] = v
		}
	}
	return out
}

func (self *LocalState) setSnClaimTxHash(epoch int64, txHash string) error {
	raw := map[string]string{}
	self.snReadJson(snClaimTxFile, &raw)
	raw[strconv.FormatInt(epoch, 10)] = txHash
	return self.snWriteJson(snClaimTxFile, raw)
}

func (self *LocalState) getSnArtifact(hashHex string) []byte {
	if !snHexRe.MatchString(hashHex) {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(self.snPath(snArtifactDir), hashHex+".json"))
	if err != nil {
		return nil
	}
	return b
}

func (self *LocalState) setSnArtifact(hashHex string, raw []byte) error {
	if !snHexRe.MatchString(hashHex) {
		return errors.New("invalid artifact hash")
	}
	dir := self.snPath(snArtifactDir)
	if err := os.MkdirAll(dir, snArtifactMode); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, hashHex+".json"), raw, LocalStorageFilePermissions)
}
