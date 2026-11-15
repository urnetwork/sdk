//go:build !ios_extension

// SidecarApprovals' device types (§18.5, D-SDK-30; SDK-M8-05): this member's devices, a revocation's plan and its rows,
// and the codes, kinds and decisions a result names. Each crosses gobind, and as json the C ABI and js, so each field
// is a string, a bool, an int32 or int64 count or milliseconds, an *Id, []byte or an exportedList, never a
// sidecar-package value (R-SDK-5). The calls and the items a person decides are sidecar_approvals.go's; this file links
// no sidecar package, so every leg but the iOS extension's, js among them, declares these once, and the js leg answers
// its alarms-only refusals in them (§18.4).
//
// The copy an app shows is §18.5's; these carry what it needs: a device's state and last change, a plan's rows,
// signatures and Presence, and a refusal's code. A device's forwarding Delegation is no device of its own: the list
// shows one device per app, and a compromise plan's rows name the Delegation among what the revocation ends, as the
// core's cascade lists it (DESIGN-DETAILS 4.7).

package sdk

import (
	"github.com/urnetwork/connect"
)

// The codes a SidecarResult's or a plan's Code carries. Format values: apps read them.
const (
	// §5.6's REVOKE_ codes.
	SidecarCodeRevokeSelf       = "REVOKE_SELF"
	SidecarCodeRevokeNotOwn     = "REVOKE_NOT_OWN"
	SidecarCodeRevokeNotCapable = "REVOKE_NOT_CAPABLE"
	SidecarCodeRevokeCancelled  = "REVOKE_CANCELLED"
	SidecarCodeRevokeSince      = "REVOKE_SINCE"
	SidecarCodeRevokeParksSelf  = "REVOKE_PARKS_SELF"
	SidecarCodeRevokeStale      = "REVOKE_STALE"
	SidecarCodeRevokeRefused    = "REVOKE_REFUSED"
	// §5.4's, as elsewhere: the sidecar is locked or unenrolled.
	SidecarCodeFailedPrecondition = "FAILED_PRECONDITION"
	// Nothing was signed: URnetwork or the daemon could not be reached, the Sidecar closed, or the device holds no
	// verified state yet.
	SidecarCodeUnavailable = "UNAVAILABLE"
	// The call names nothing it can act on: no device, plan or reason it takes, or a key no row of the plan offers.
	SidecarCodeInvalidArgument = "INVALID_ARGUMENT"
	// No item of that id waits for this device: it was decided elsewhere, closed, or never listed here.
	SidecarCodeApprovalUnknown = "APPROVAL_UNKNOWN"
	// This Sidecar decided the item already, or is deciding it: each item takes one decision (§16.8).
	SidecarCodeApprovalDecided = "APPROVAL_DECIDED"
	// The item does not take that decision, or this device may not make it: a read_only, ephemeral or tagged device,
	// one not consented for the member, or one not of an AdminSet member where the kind asks for one.
	SidecarCodeApprovalNotEligible = "APPROVAL_NOT_ELIGIBLE"
	// The decision needs more counted signers than this device can gather yet (§7.9's counted rule).
	SidecarCodeApprovalAuthority = "APPROVAL_AUTHORITY"
	// The core or the daemon refused the decision for another reason, which Message names.
	SidecarCodeApprovalRefused = "APPROVAL_REFUSED"
)

// SidecarApprovalItem.Kind's values (§18.5's GetPending). Format values: apps read them.
const (
	// §16.8's requests whose approvers name this device's member.
	SidecarApprovalKindRequest = "request"
	// A new device of this device's member, after its safety number.
	SidecarApprovalKindAdmission = "admission"
	// §5.6's admission alarm, a parked device's included.
	SidecarApprovalKindAdmissionAlarm = "admission_alarm"
	// An AdminSet device's veto of a recovery window (§7.9).
	SidecarApprovalKindRecoveryWindow = "recovery_window"
	// A recovery binding rebound: not me, or I did this.
	SidecarApprovalKindRecoveryRebind = "recovery_rebind"
	// A device a recovery admitted that this device may revoke.
	SidecarApprovalKindRecoveryUnseen = "recovery_unseen"
	// §16.11's access requests this member approves.
	SidecarApprovalKindAccessRequest = "access_request"
	// Another AdminSet member's device revocation proposal this device may cosign (§5.6).
	SidecarApprovalKindRevocationProposal = "revocation_proposal"
	// An admin device's cleanup list near config_admissions' cap (§7.10's cleanup row).
	SidecarApprovalKindAdmissionsNearCap = "admissions_near_cap"
)

// The decisions an item takes.
const (
	SidecarDecisionApprove = "approve"
	SidecarDecisionDeny    = "deny"
	SidecarDecisionRevoke  = "revoke"
)

// SidecarDevice.State's values (§5.4's MemberDevice).
const (
	SidecarDeviceLive    = "live"
	SidecarDevicePending = "pending"
	SidecarDeviceParked  = "parked"
	SidecarDeviceStale   = "stale"
	SidecarDeviceRevoked = "revoked"
)

// PreviewRevokeDevice's reasons, SidecarRevoke's own (§7.9).
const (
	SidecarRevokeLost       = "lost"
	SidecarRevokeCompromise = "compromise"
)

// One of this member's admitted devices (§5.4's MemberDevice). json in the C ABI and js.
type SidecarDevice struct {
	SidecarId *Id `json:"sidecar_id"`
	// Its config device name.
	Name string `json:"name,omitempty"`
	// This device: Sign out, never Revoke.
	This bool `json:"this,omitempty"`
	// A page Worker.
	ReadOnly bool `json:"read_only,omitempty"`
	// A quick burner.
	Ephemeral bool `json:"ephemeral,omitempty"`
	// live, pending (suspended while its revocation awaits cosigns), parked, stale or revoked.
	State string `json:"state"`
	// In unix milliseconds; 0 for none known.
	AdmittedMillis int64 `json:"admitted_millis,omitempty"`
	// Its newest leaf change this device shows, in unix milliseconds; 0: none seen.
	LastChangeMillis int64 `json:"last_change_millis,omitempty"`
}

// This member's devices, this one first.
type SidecarDeviceList struct {
	exportedList[*SidecarDevice]
}

// An empty list.
func NewSidecarDeviceList() *SidecarDeviceList {
	return &SidecarDeviceList{
		exportedList: *newExportedList[*SidecarDevice](),
	}
}

// One row of a plan (§5.4's DeviceRevokeItem). json in the C ABI and js.
type SidecarRevokeItem struct {
	// park, void, stands, reseal or key.
	Effect string `json:"effect"`
	// What the row names: device, delegation, recovery_binding, key, evs_share, grant, confirmation and so on.
	Kind string `json:"kind"`
	// A device's or key's name, or an EvsShare's repo.
	Name string `json:"name,omitempty"`
	// An id's 16 bytes, a recovery_binding's 32-byte key hash or a key's key_id.
	Id []byte `json:"id,omitempty"`
	// The entry that carries it.
	Position int64 `json:"position,omitempty"`
}

// A plan's rows, in the plan's order.
type SidecarRevokeItemList struct {
	exportedList[*SidecarRevokeItem]
}

// An empty list.
func NewSidecarRevokeItemList() *SidecarRevokeItemList {
	return &SidecarRevokeItemList{
		exportedList: *newExportedList[*SidecarRevokeItem](),
	}
}

// §5.6's plan of revoking another of this member's devices (§5.4's DeviceRevokePlan), which a person reads before
// RevokeDevice signs it. A refusal is a plan with Code set and no PlanId. json in the C ABI and js.
type SidecarRevokePlan struct {
	PlanId    *Id `json:"plan_id,omitempty"`
	SidecarId *Id `json:"sidecar_id,omitempty"`
	// The device's config device name.
	Name string `json:"name,omitempty"`
	// lost or compromise.
	Reason string `json:"reason,omitempty"`
	// compromise: where P starts, and its entry's time in unix milliseconds.
	SinceN      int64 `json:"since_n,omitempty"`
	SinceMillis int64 `json:"since_millis,omitempty"`
	// §5.4's rows: park, void, stands, reseal, key.
	Items *SidecarRevokeItemList `json:"items,omitempty"`
	// 1, or k for an AdminSet member's device.
	Signatures int32 `json:"signatures,omitempty"`
	// RevokeDevice runs SignWithPresence.
	Presence bool `json:"presence,omitempty"`
	// A refusal: one of the REVOKE_ codes, FAILED_PRECONDITION, UNAVAILABLE or INVALID_ARGUMENT; empty on a plan.
	Code string `json:"code,omitempty"`
	// REVOKE_PARKS_SELF: the entry that puts this device in P.
	ParksSelfPosition int64 `json:"parks_self_position,omitempty"`
	// A refusal's text, for a person. Never a key or a secret (I-7).
	Message string `json:"message,omitempty"`
}

// Answers once, with a plan or a refusal in it, or an error when the call could not run.
type SidecarRevokePlanCallback connect.ApiCallback[*SidecarRevokePlan]
