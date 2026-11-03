//go:build !ios_extension && !js

package sdk

// The server's side of claiming a Top 200 spot with one tap (apple BTEASY.md,
// "Claiming a Top 200 spot"): the wire shapes and calls behind SnFleetDevice
// (sn_fleet_device.go) and SnFleetClaim (sn_fleet_claim.go). Apps use those two
// rather than these calls.
//
// Every answer is checked before anything is signed from it. The server chooses
// devices and carries messages. It decides nothing a device or a key signs.

import (
	"context"
	"errors"
	"fmt"

	"github.com/urnetwork/connect"
)

// A text and a wallet's signature over it.
type SnFleetSignedMessage struct {
	Message string `json:"message"`
	// 0x hex, 64 bytes
	Signature string `json:"signature"`
}

// What the server asks of a device (GET /sn/fleet/device). Everything but
// BindingOpen and NextCheckSeconds is usually empty: most devices aren't chosen.
type SnFleetDeviceWork struct {
	BindingOpen bool `json:"binding_open"`
	// the server's choice of when to ask again, which the device clamps
	NextCheckSeconds int64 `json:"next_check_seconds"`
	// the network's newest consent, sent when the device asked for it
	Consent *SnFleetSignedMessage `json:"consent,omitempty"`
	// the coldkey's authorization of the hotkey named in the binding
	Authorization *SnFleetSignedMessage `json:"authorization,omitempty"`
	// the binding to sign, as json, with the hotkey's signature over its digest
	BindingJson     string `json:"binding_json,omitempty"`
	HotkeySignature string `json:"hotkey_signature,omitempty"`
}

// A device's answer to a binding: its signature, or why it refused.
type SnFleetDeviceSignatureArgs struct {
	Digest          string `json:"digest"`
	ClientSignature string `json:"client_signature,omitempty"`
	Refusal         string `json:"refusal,omitempty"`
}

type SnFleetDeviceSignatureResult struct {
	Accepted bool     `json:"accepted"`
	Error    *SnError `json:"error,omitempty"`
}

// The device's newest binding on chain, for leaving (GET /sn/fleet/device/binding).
type SnFleetDeviceBinding struct {
	Bound          bool   `json:"bound"`
	ChainId        int64  `json:"chain_id"`
	Netuid         int64  `json:"netuid"`
	Coordinator    string `json:"coordinator"`
	Generation     int64  `json:"generation"`
	ValidFromEpoch int64  `json:"valid_from_epoch"`
	ValidToEpoch   int64  `json:"valid_to_epoch"`
	CurrentEpoch   int64  `json:"current_epoch"`
}

type SnFleetDeviceLeaveArgs struct {
	Generation      int64  `json:"generation"`
	EffectiveEpoch  int64  `json:"effective_epoch"`
	ClientSignature string `json:"client_signature,omitempty"`
}

// How a Leave went. The server sends the revocation through its relay.
type SnFleetDeviceLeaveResult struct {
	Left bool `json:"left"`
	// the first epoch the device is back in the pool; 0 when it was never bound
	EffectiveEpoch int64 `json:"effective_epoch"`
	// the epoch moved on before the request arrived: sign again for this one
	CurrentEpoch int64    `json:"current_epoch"`
	Error        *SnError `json:"error,omitempty"`
}

// One binding for the key holder's device to sign with the hotkey.
type SnFleetHotkeyWork struct {
	BindingJson string `json:"binding_json"`
}

// The network's manifest in its newest generation.
type SnFleetManifestState struct {
	Generation int64 `json:"generation"`
	// the exact canonical bytes the hotkey publishes the hash of
	Manifest string `json:"manifest"`
	// the coordinator has mirrored this manifest's commitment
	Mirrored bool `json:"mirrored"`
}

// How many of the network's devices are where.
type SnFleetDeviceCounts struct {
	// the server chose these, one per IP prefix, up to the per-fleet share
	Chosen int64 `json:"chosen"`
	// chosen devices that signed, or that are bound already
	Ready int64 `json:"ready"`
	// chosen devices bound on chain, including those starting next epoch
	Bound int64 `json:"bound"`
	// chosen devices that refused; the next batch replaces them
	Refused int64 `json:"refused"`
	// the network's online providers that haven't asked for work: an older
	// release, or auto-signing turned off
	StillToUpdate int64 `json:"still_to_update"`
}

// A top-up the server sent, or why there can't be one.
type SnFleetTopUp struct {
	// the network has had its one top-up
	Used bool `json:"used"`
	// a request now would be accepted, given the funded key and the board's prompt
	Available bool  `json:"available"`
	AmountRao int64 `json:"amount_rao"`
	// sent and final on chain
	Landed bool `json:"landed"`
}

// The network's claim as the server sees it (GET /sn/fleet/claim).
type SnFleetClaimStatus struct {
	BindingOpen bool `json:"binding_open"`
	// the board prompts this network to claim a spot
	ClaimPrompt bool `json:"claim_prompt"`
	// the newest consent's coldkey, "" without a consent
	ConsentColdkey string `json:"consent_coldkey"`
	ChainId        int64  `json:"chain_id"`
	Netuid         int64  `json:"netuid"`
	Coordinator    string `json:"coordinator"`
	// the authorization the server holds, "" without one
	AuthorizationMessage string `json:"authorization_message"`
	// the server's newest census of the hotkey's UID
	Registered bool  `json:"registered"`
	Uid        int64 `json:"uid"`
	// the settlement epoch, and when the next one starts
	CurrentEpoch        int64 `json:"current_epoch"`
	CurrentBlock        int64 `json:"current_block"`
	NextEpochStartBlock int64 `json:"next_epoch_start_block"`
	// a registration here keeps the new UID immune until its binding starts,
	// and leaves time to bind before that epoch
	RegisterAfterBlock  int64 `json:"register_after_block"`
	RegisterBeforeBlock int64 `json:"register_before_block"`
	// the newest generation, nil before the first
	Manifest *SnFleetManifestState `json:"manifest,omitempty"`
	// the bindings waiting for the hotkey's signature
	//gomobile:noexport []*SnFleetHotkeyWork, read by SnFleetClaim
	HotkeyWork []*SnFleetHotkeyWork `json:"hotkey_work,omitempty"`
	Counts     SnFleetDeviceCounts  `json:"counts"`
	TopUp      SnFleetTopUp         `json:"top_up"`
}

type SnFleetAuthorizationArgs struct {
	Message   string `json:"message"`
	Signature string `json:"signature"`
}

type SnFleetAuthorizationResult struct {
	Accepted bool     `json:"accepted"`
	Error    *SnError `json:"error,omitempty"`
}

// One hotkey signature, by the digest the key holder's device computed itself.
type SnFleetHotkeySignature struct {
	Digest          string `json:"digest"`
	HotkeySignature string `json:"hotkey_signature"`
}

type SnFleetHotkeySignaturesArgs struct {
	//gomobile:noexport []*SnFleetHotkeySignature, sent by SnFleetClaim
	Signatures []*SnFleetHotkeySignature `json:"signatures"`
}

type SnFleetHotkeySignaturesResult struct {
	Accepted int64    `json:"accepted"`
	Error    *SnError `json:"error,omitempty"`
}

type SnFleetTopUpResult struct {
	AmountRao int64    `json:"amount_rao"`
	Error     *SnError `json:"error,omitempty"`
}

// snFleetServer is what the flows need from the server. Api implements it; tests
// supply their own.
type snFleetServer interface {
	snFleetDeviceWork(ctx context.Context, needConsent bool) (*SnFleetDeviceWork, error)
	snFleetDeviceSignature(ctx context.Context, args *SnFleetDeviceSignatureArgs) (*SnFleetDeviceSignatureResult, error)
	snFleetDeviceBinding(ctx context.Context) (*SnFleetDeviceBinding, error)
	snFleetDeviceLeave(ctx context.Context, args *SnFleetDeviceLeaveArgs) (*SnFleetDeviceLeaveResult, error)
	snFleetClaimStatus(ctx context.Context) (*SnFleetClaimStatus, error)
	snFleetAuthorize(ctx context.Context, args *SnFleetAuthorizationArgs) (*SnFleetAuthorizationResult, error)
	snFleetHotkeySignatures(ctx context.Context, args *SnFleetHotkeySignaturesArgs) (*SnFleetHotkeySignaturesResult, error)
	snFleetTopUp(ctx context.Context) (*SnFleetTopUpResult, error)
	snFleetByJwt() string
}

func (self *Api) snFleetByJwt() string {
	return self.GetByJwt()
}

func (self *Api) snFleetDeviceWork(ctx context.Context, needConsent bool) (*SnFleetDeviceWork, error) {
	query := ""
	if needConsent {
		query = "?need_consent=1"
	}
	return connect.HttpGetWithRawFunction(
		ctx,
		self.getHttpGetRaw(),
		fmt.Sprintf("%s/sn/fleet/device%s", self.apiUrl, query),
		self.GetByJwt(),
		&SnFleetDeviceWork{},
		connect.NewNoopApiCallback[*SnFleetDeviceWork](),
	)
}

func (self *Api) snFleetDeviceSignature(ctx context.Context, args *SnFleetDeviceSignatureArgs) (*SnFleetDeviceSignatureResult, error) {
	return connect.HttpPostWithRawFunction(
		ctx,
		self.getHttpPostRaw(),
		fmt.Sprintf("%s/sn/fleet/device/signature", self.apiUrl),
		args,
		self.GetByJwt(),
		&SnFleetDeviceSignatureResult{},
		connect.NewNoopApiCallback[*SnFleetDeviceSignatureResult](),
	)
}

func (self *Api) snFleetDeviceBinding(ctx context.Context) (*SnFleetDeviceBinding, error) {
	return connect.HttpGetWithRawFunction(
		ctx,
		self.getHttpGetRaw(),
		fmt.Sprintf("%s/sn/fleet/device/binding", self.apiUrl),
		self.GetByJwt(),
		&SnFleetDeviceBinding{},
		connect.NewNoopApiCallback[*SnFleetDeviceBinding](),
	)
}

func (self *Api) snFleetDeviceLeave(ctx context.Context, args *SnFleetDeviceLeaveArgs) (*SnFleetDeviceLeaveResult, error) {
	return connect.HttpPostWithRawFunction(
		ctx,
		self.getHttpPostRaw(),
		fmt.Sprintf("%s/sn/fleet/device/leave", self.apiUrl),
		args,
		self.GetByJwt(),
		&SnFleetDeviceLeaveResult{},
		connect.NewNoopApiCallback[*SnFleetDeviceLeaveResult](),
	)
}

func (self *Api) snFleetClaimStatus(ctx context.Context) (*SnFleetClaimStatus, error) {
	return connect.HttpGetWithRawFunction(
		ctx,
		self.getHttpGetRaw(),
		fmt.Sprintf("%s/sn/fleet/claim", self.apiUrl),
		self.GetByJwt(),
		&SnFleetClaimStatus{},
		connect.NewNoopApiCallback[*SnFleetClaimStatus](),
	)
}

// The authorization, hotkey signatures and top-up act for the whole network, so
// they take the network credential, as the consent does.
func (self *Api) snFleetAuthorize(ctx context.Context, args *SnFleetAuthorizationArgs) (*SnFleetAuthorizationResult, error) {
	byJwt := self.networkCredential()
	if byJwt == "" {
		return nil, ErrNetworkCredentialRequired
	}
	return connect.HttpPostWithRawFunction(
		ctx,
		self.getHttpPostRaw(),
		fmt.Sprintf("%s/sn/fleet/authorization", self.apiUrl),
		args,
		byJwt,
		&SnFleetAuthorizationResult{},
		connect.NewNoopApiCallback[*SnFleetAuthorizationResult](),
	)
}

func (self *Api) snFleetHotkeySignatures(ctx context.Context, args *SnFleetHotkeySignaturesArgs) (*SnFleetHotkeySignaturesResult, error) {
	byJwt := self.networkCredential()
	if byJwt == "" {
		return nil, ErrNetworkCredentialRequired
	}
	return connect.HttpPostWithRawFunction(
		ctx,
		self.getHttpPostRaw(),
		fmt.Sprintf("%s/sn/fleet/hotkey-signatures", self.apiUrl),
		args,
		byJwt,
		&SnFleetHotkeySignaturesResult{},
		connect.NewNoopApiCallback[*SnFleetHotkeySignaturesResult](),
	)
}

func (self *Api) snFleetTopUp(ctx context.Context) (*SnFleetTopUpResult, error) {
	byJwt := self.networkCredential()
	if byJwt == "" {
		return nil, ErrNetworkCredentialRequired
	}
	return connect.HttpPostWithRawFunction(
		ctx,
		self.getHttpPostRaw(),
		fmt.Sprintf("%s/sn/fleet/top-up", self.apiUrl),
		nil,
		byJwt,
		&SnFleetTopUpResult{},
		connect.NewNoopApiCallback[*SnFleetTopUpResult](),
	)
}

// errSnFleetNoResult is a server that answered nothing where the flow needs an
// answer: treated like any failed call.
var errSnFleetNoResult = errors.New("the server returned no result")
