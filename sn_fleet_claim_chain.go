//go:build !ios_extension && !js

package sdk

// The chain side of SnFleetClaim. The sdk's Substrate client belongs to the convert
// work (BTEASY.md, "Convert SN25α to TAO"). Until it lands, a claim stops here and
// says the chain isn't available, rather than signing anything.

import (
	"context"
	"errors"
)

var errSnFleetClaimNoChain = errors.New("this build has no Substrate client")

type snFleetClaimNoChain struct{}

func newSnFleetClaimDefaultChain(_ *Api) snFleetClaimChain {
	return snFleetClaimNoChain{}
}

func (snFleetClaimNoChain) Block(context.Context) (uint64, error) {
	return 0, errSnFleetClaimNoChain
}

func (snFleetClaimNoChain) HotkeyUid(context.Context, uint16, [32]byte) (int64, bool, error) {
	return 0, false, errSnFleetClaimNoChain
}

func (snFleetClaimNoChain) RegistrationBurnRao(context.Context, uint16) (uint64, error) {
	return 0, errSnFleetClaimNoChain
}

func (snFleetClaimNoChain) RegistrationFeeRao(context.Context, uint16, [32]byte, [32]byte, uint64) (uint64, error) {
	return 0, errSnFleetClaimNoChain
}

func (snFleetClaimNoChain) FreeBalanceRao(context.Context, [32]byte) (uint64, error) {
	return 0, errSnFleetClaimNoChain
}

func (snFleetClaimNoChain) ConvertibleRao(context.Context, uint16, [32]byte) (uint64, error) {
	return 0, errSnFleetClaimNoChain
}

func (snFleetClaimNoChain) FleetCommitment(context.Context, uint16, [32]byte) ([32]byte, bool, error) {
	return [32]byte{}, false, errSnFleetClaimNoChain
}

func (snFleetClaimNoChain) ConvertAlpha(context.Context, string, uint16, uint64) error {
	return errSnFleetClaimNoChain
}

func (snFleetClaimNoChain) RegisterLimit(context.Context, string, uint16, [32]byte, uint64) error {
	return errSnFleetClaimNoChain
}

func (snFleetClaimNoChain) SetFleetCommitment(context.Context, [32]byte, uint16, [32]byte) error {
	return errSnFleetClaimNoChain
}

// The recovery phrase's two operations come from bittensor_local_key.go, the local
// key work, when it lands. Until then a key on this device can't sign here, and the
// claim asks for a wallet.
type snFleetClaimLocalColdkey struct{}

func (snFleetClaimLocalColdkey) Address(string) (string, error) {
	return "", errors.New("this build has no local key")
}

func (snFleetClaimLocalColdkey) SignMessage(string, string) (string, error) {
	return "", errors.New("this build has no local key")
}
