package sdk

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/urnetwork/sdk/sn/evm"
	"github.com/urnetwork/sdk/sn/merkle"
	"github.com/urnetwork/sdk/sn/ss58"
)

// UR protocol (subnet 25) helpers shared by every app: address checks and
// display formatting. Everything here is pure; nothing touches the network.

// Bittensor coldkeys use the generic substrate ss58 prefix.
const SnSs58Prefix = 42

// 1 alpha = 1e9 rao.
const SnRaoPerAlpha int64 = 1_000_000_000

// SnAlphaSymbol is the display symbol for subnet 25 alpha.
const SnAlphaSymbol = "SN25α"

// Stable error codes carried in SnError.Code (and as the prefix of
// SnClaimCallback.Failed messages) so the apps can localize them.
const (
	SnErrorCodeInvalidAddress      = "invalid_ss58_address"
	SnErrorCodeWalletBlocked       = "wallet_blocked"
	SnErrorCodeConnectWalletFirst  = "connect_wallet_first"
	SnErrorCodeChainNotConfigured  = "chain_not_configured"
	SnErrorCodeChainRpcUnreachable = "chain_rpc_unreachable"
	SnErrorCodeChainRpcError       = "chain_rpc_error"
	SnErrorCodeNeedsGas            = "needs_gas"
	SnErrorCodeExpired             = "claims_for_epoch_expired"
	SnErrorCodeAlreadyClaimed      = "already_claimed"
	SnErrorCodeNotClaimable        = "not_claimable"
	SnErrorCodeArtifactUnavailable = "artifact_unavailable"
	SnErrorCodeProofMismatch       = "proof_mismatch"
	SnErrorCodeClaimFailed         = "claim_failed"
	SnErrorCodeLocalState          = "local_state_unavailable"
	SnErrorCodeServer              = "server_error"
)

// SnError is the error shape of every sn result in this package.
type SnError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

func newSnError(code string, message string) *SnError {
	if message == "" {
		message = code
	}
	return &SnError{Code: code, Message: message}
}

// snErrorFromErr maps a Go error to a coded SnError.
func snErrorFromErr(err error) *SnError {
	if err == nil {
		return nil
	}
	var rpcErr *evm.RpcError
	switch {
	case errors.Is(err, evm.ErrUnreachable):
		return newSnError(SnErrorCodeChainRpcUnreachable, err.Error())
	case errors.As(err, &rpcErr):
		return newSnError(SnErrorCodeChainRpcError, rpcErr.Reason())
	default:
		return newSnError(SnErrorCodeServer, err.Error())
	}
}

// ValidateSs58 reports whether address is a well-formed Bittensor (prefix 42)
// ss58 address. This is the local syntax check; the server's
// POST /sn/wallet/validate adds the chain and ban checks.
func ValidateSs58(address string) bool {
	_, err := ss58.DecodeWithPrefix(strings.TrimSpace(address), SnSs58Prefix)
	return err == nil
}

// ShortSs58 abbreviates an address for display: "5F3s…kQ9v".
func ShortSs58(address string) string {
	address = strings.TrimSpace(address)
	if len(address) <= 12 {
		return address
	}
	return address[:4] + "…" + address[len(address)-4:]
}

// EvmMirrorSs58 returns the ss58 mirror of an 0x EVM address: the substrate
// account that funds the EVM account (send TAO here to pay gas). Returns ""
// for an invalid address.
func EvmMirrorSs58(address string) string {
	h160, err := evm.ParseAddress(address)
	if err != nil {
		return ""
	}
	mirror, err := ss58.EvmMirrorAddress(h160, SnSs58Prefix)
	if err != nil {
		return ""
	}
	return mirror
}

// AlphaFromRao converts rao to alpha as a float for charts.
func AlphaFromRao(rao int64) float64 {
	return float64(rao) / float64(SnRaoPerAlpha)
}

// FormatAlphaAmount renders rao as alpha with four decimals: "3.2410".
func FormatAlphaAmount(rao int64) string {
	sign := ""
	if rao < 0 {
		sign = "-"
		rao = -rao
	}
	whole := rao / SnRaoPerAlpha
	frac := rao % SnRaoPerAlpha
	// round half up to 1e-4 alpha (1e5 rao)
	tenThousandths := (frac + 50_000) / 100_000
	if tenThousandths == 10_000 {
		whole += 1
		tenThousandths = 0
	}
	return fmt.Sprintf("%s%d.%04d", sign, whole, tenThousandths)
}

// FormatAlpha renders rao with the symbol: "3.2410 SN25α".
func FormatAlpha(rao int64) string {
	return FormatAlphaAmount(rao) + " " + SnAlphaSymbol
}

// FormatShareBps renders basis points as a percentage: 71 -> "0.71%".
func FormatShareBps(shareBps int64) string {
	return fmt.Sprintf("%.2f%%", float64(shareBps)/100)
}

// snColdkeyBytes decodes a Bittensor address to its 32-byte public key.
func snColdkeyBytes(address string) ([32]byte, error) {
	return ss58.DecodeWithPrefix(strings.TrimSpace(address), SnSs58Prefix)
}

// SnPayoutLeafHex renders the vault leaf for a coldkey and share, for hosts
// that verify or display proofs themselves.
func SnPayoutLeafHex(coldkeySs58 string, shareBps int64) string {
	coldkey, err := snColdkeyBytes(coldkeySs58)
	if err != nil || shareBps < 0 {
		return ""
	}
	leaf := merkle.PayoutLeaf(coldkey, big.NewInt(shareBps))
	return "0x" + hex.EncodeToString(leaf[:])
}

// verifyPayoutProof checks an OpenZeppelin-style sorted-pair proof over raw
// bytes. Not bound: gomobile cannot bind [][]byte, use VerifyPayoutProofHex.
func verifyPayoutProof(root []byte, leaf []byte, proof [][]byte) bool {
	if len(root) != 32 || len(leaf) != 32 {
		return false
	}
	var r [32]byte
	copy(r[:], root)
	var l merkle.Leaf
	copy(l[:], leaf)
	nodes := make([][32]byte, 0, len(proof))
	for _, node := range proof {
		if len(node) != 32 {
			return false
		}
		var n [32]byte
		copy(n[:], node)
		nodes = append(nodes, n)
	}
	return merkle.Verify(r, l, nodes)
}

// VerifyPayoutProofHex checks an OpenZeppelin-style sorted-pair proof given
// as hex strings.
func VerifyPayoutProofHex(rootHex string, leafHex string, proofHex *StringList) bool {
	root, err := evm.ParseHexBytes(rootHex)
	if err != nil {
		return false
	}
	leaf, err := evm.ParseHexBytes(leafHex)
	if err != nil {
		return false
	}
	var proof [][]byte
	if proofHex != nil {
		for i := 0; i < proofHex.Len(); i++ {
			node, err := evm.ParseHexBytes(proofHex.Get(i))
			if err != nil {
				return false
			}
			proof = append(proof, node)
		}
	}
	return verifyPayoutProof(root, leaf, proof)
}
