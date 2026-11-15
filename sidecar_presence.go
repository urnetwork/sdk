//go:build !ios_extension && !js

// SignWithPresence wiring (§18.5's SidecarPresenceSigner, D-SDK-30, §5.6's approval and presence countersignature,
// §6.9's WebAuthnEvidence; owner decisions 185, 187; SDK-M8-05): the app's presence key, a Secure Enclave or platform
// FIDO key, signs the act's countersigned bytes wherever §7.9's presence row covers an act this device signs. Whether
// it does is the log's, never the caller's: nothing here decides that an act needs presence or may skip it.
//
//	local    sidecarPresenceCheckOf: the core's act calls the check with its countersigned bytes before it signs any
//	         statement (identity_act.go). The app's SignWithPresence gets them as standard base64 and answers standard
//	         base64 of §6.9's WebAuthnEvidence, encoded, which the act verifies against a live presence key of this
//	         sidecar's before it signs. "" is the person's refusal; a panic, an answer that does not decode, or no
//	         signer at all is the same, and the act signs nothing (REVOKE_CANCELLED)
//	remote   §5.8's two calls (sidecarRemotePresenceAct): the first, confirmed, makes the daemon build the act and
//	         answer its countersigned bytes and an act id, signing nothing; the app signs the bytes here; the second,
//	         confirmed, sends the evidence back under that id, and only then does the daemon sign, each statement
//	         carrying the countersignature. A first answer with no bytes is an act the log does not cover, which the
//	         second call signs on the person's confirmation alone. An empty signature, or no signer, ends it after the
//	         first call, so the daemon signs nothing and the act it built lapses
//	wire     for a revocation, devices.revoke.begin takes devices.revoke's params ({network, plan_id, revoke_key_ids})
//	         and answers {act_id, countersigned, refusal}; devices.revoke.finish takes {network, act_id, evidence} and
//	         answers devices.revoke's reply. CORE-M8-03 serves both (this is the shape the sdk calls); a daemon from
//	         before them answers the first method not found, and the app then takes the one confirmed devices.revoke,
//	         under the daemon's own presence prompt
//
// The bytes and the evidence go only between the core or the daemon and the app's signer: nothing here logs either.

package sdk

import (
	"context"
	"encoding/base64"
	"errors"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

var (
	// The app has no presence signer, so another device approves: an act the log covers is not signed here.
	errSidecarPresenceNoSigner = errors.New("this app holds no presence key, so another device approves")
	// The person refused the presence check, or the key failed: SignWithPresence answered "".
	errSidecarPresenceRefused = errors.New("the presence check was refused")
	// SignWithPresence answered something that is no base64 of an encoded WebAuthnEvidence naming a key.
	errSidecarPresenceEvidence = errors.New("the presence key's answer is no WebAuthnEvidence")
)

// Calls signer's SignWithPresence over message, under trace, since a panic that crosses the binding would exit the
// app: the evidence's encoded bytes, or an error for no signer, a refusal, a panic or an answer that does not decode.
// The bytes are checked as far as their shape: a key_id and a signature. Whether they verify is the act's to say.
func sidecarSignWithPresence(signer SidecarPresenceSigner, message []byte) ([]byte, error) {
	if signer == nil {
		return nil, errSidecarPresenceNoSigner
	}
	answer, err := traceWithReturn(func() string {
		return signer.SignWithPresence(base64.StdEncoding.EncodeToString(message))
	})
	if err != nil {
		return nil, errSidecarPresenceRefused
	}
	if answer == "" {
		return nil, errSidecarPresenceRefused
	}
	encoded, err := base64.StdEncoding.Strict().DecodeString(answer)
	if err != nil {
		return nil, errSidecarPresenceEvidence
	}
	evidence := &bridge.WebAuthnEvidence{}
	if err := proto.Unmarshal(encoded, evidence); err != nil || len(evidence.GetKeyId()) == 0 || len(evidence.GetSignature()) == 0 {
		return nil, errSidecarPresenceEvidence
	}
	return encoded, nil
}

// The core's presence check over the app's signer: the act's countersigned bytes in, §6.9's evidence out. Nil for no
// signer, so an act the log covers answers ErrSidecarActPresence with nothing signed, as it does for a refusal.
func sidecarPresenceCheckOf(signer SidecarPresenceSigner) sidecar.SidecarPresenceCheck {
	if signer == nil {
		return nil
	}
	return func(countersignedBytes []byte) (*bridge.WebAuthnEvidence, error) {
		encoded, err := sidecarSignWithPresence(signer, countersignedBytes)
		if err != nil {
			return nil, err
		}
		evidence := &bridge.WebAuthnEvidence{}
		if err := proto.Unmarshal(encoded, evidence); err != nil {
			return nil, errSidecarPresenceEvidence
		}
		return evidence, nil
	}
}

// The first of §5.8's two calls' answer: the act the daemon built and its countersigned bytes, or §5.6's refusal. The
// shape a daemon serving the two calls answers (CORE-M8-03).
type sidecarControlPresenceBeginReply struct {
	// What the second call names the act by: the daemon keeps the act's statements under it until then.
	ActId *Id `json:"act_id,omitempty"`
	// The act's countersigned bytes (§7.9's presence row): empty for an act the log does not cover.
	Countersigned []byte `json:"countersigned,omitempty"`
	// §5.6's refusal before anything was built.
	Refusal *sidecarControlDeviceRefusal `json:"refusal,omitempty"`
}

// The second call's params: the act, and the evidence, empty for an act the log does not cover.
type sidecarControlPresenceFinishParams struct {
	Network string `json:"network,omitempty"`
	ActId   *Id    `json:"act_id"`
	// §6.9's WebAuthnEvidence, encoded, as SignWithPresence answered it.
	Evidence []byte `json:"evidence,omitempty"`
}

// The daemon answered the first call with neither an act nor a refusal.
var errSidecarPresenceBeginReply = errors.New("the daemon's first call of the act named no act")

// §5.8's two calls (the file's comment): beginMethod with beginParams, confirmed, then, once signer signed the
// countersigned bytes the daemon answered, finishMethod with the evidence, confirmed, decoding its answer into result.
// §5.6's refusal of the first call is answered in refusal, with nothing sent after it. A refusal of the presence check,
// or no signer where the act has bytes to sign, ends the act after the first call with errSidecarPresenceRefused or
// errSidecarPresenceNoSigner, nothing signed.
func sidecarRemotePresenceAct(
	ctx context.Context,
	remote sidecarBackend,
	network string,
	beginMethod string,
	beginParams any,
	finishMethod string,
	signer SidecarPresenceSigner,
	result any,
) (refusal *sidecarControlDeviceRefusal, err error) {
	begin := &sidecarControlPresenceBeginReply{}
	if err := remote.call(ctx, beginMethod, beginParams, true, begin); err != nil {
		return nil, err
	}
	if begin.Refusal != nil {
		return begin.Refusal, nil
	}
	if begin.ActId == nil {
		return nil, errSidecarPresenceBeginReply
	}
	finish := &sidecarControlPresenceFinishParams{Network: network, ActId: begin.ActId}
	if 0 < len(begin.Countersigned) {
		evidence, err := sidecarSignWithPresence(signer, begin.Countersigned)
		if err != nil {
			// the daemon signs nothing: the act it built lapses unsent
			return nil, err
		}
		finish.Evidence = evidence
	}
	return nil, remote.call(ctx, finishMethod, finish, true, result)
}
