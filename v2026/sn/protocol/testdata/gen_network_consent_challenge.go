//go:build ignore

// Writes network-consent-challenge-v1.json: one network consent challenge
// message exactly as the server issues it (sn protocol
// SignProspectiveNetworkWalletMapping, then Message), with a synthetic
// operator key and synthetic identities. Run it from the sn repository:
//
//	go run ../sdk/sn/protocol/testdata/gen_network_consent_challenge.go > ../sdk/sn/protocol/testdata/network-consent-challenge-v1.json
package main

import (
	"encoding/json"
	"os"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"
	"github.com/urnetwork/connect/v2026"
)

func fill(b byte) (out [32]byte) {
	for i := range out {
		out[i] = b + byte(i)
	}
	return out
}

func main() {
	keyBytes := fill(0x11)
	key, err := crypto.ToECDSA(keyBytes[:])
	if err != nil {
		panic(err)
	}
	networkId, err := connect.ParseId("00000000-0000-0000-0000-00000000a001")
	if err != nil {
		panic(err)
	}
	userId, err := connect.ParseId("00000000-0000-0000-0000-00000000b001")
	if err != nil {
		panic(err)
	}
	const currentEpoch = 42
	statement := protocol.NetworkWalletMappingStatement{
		Domain: protocol.ClientKeyHistoryDomain{
			ChainID:          964,
			GenesisHash:      fill(0xa0),
			Netuid:           25,
			Coordinator:      common.HexToAddress("0x0000000000000000000000000000000000000007"),
			SettlementVault:  common.HexToAddress("0x0000000000000000000000000000000000000008"),
			DeploymentIDHash: fill(0xb0),
			PolicyHash:       fill(0xc0),
			NoID:             7,
		},
		UserId:       [16]byte(userId),
		NetworkId:    [16]byte(networkId),
		Coldkey:      fill(0x40),
		Generation:   1,
		Nonce:        fill(0xd0),
		IssuedAt:     1791244800,
		ExpiresAt:    1791244800 + 300,
		FromEpoch:    currentEpoch + 1,
		ThroughEpoch: currentEpoch + 1 + 65535,
	}
	boundary := protocol.ClientKeyEffectiveBoundary{Epoch: currentEpoch, Block: 2116800, Hash: fill(0xe0)}
	if err := protocol.SignProspectiveNetworkWalletMapping(&statement, boundary, key); err != nil {
		panic(err)
	}
	message, err := statement.Message()
	if err != nil {
		panic(err)
	}
	if _, err := protocol.DecodeNetworkWalletMappingStatement(message); err != nil {
		panic(err)
	}
	coldkeySs58, err := ss58.Encode(statement.Coldkey, ss58.BittensorPrefix)
	if err != nil {
		panic(err)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(map[string]any{
		"network_id":    networkId.String(),
		"coldkey_ss58":  coldkeySs58,
		"current_epoch": currentEpoch,
		"from_epoch":    statement.FromEpoch,
		"through_epoch": statement.ThroughEpoch,
		"expires_at":    statement.ExpiresAt,
		"message":       message,
	}); err != nil {
		panic(err)
	}
}
