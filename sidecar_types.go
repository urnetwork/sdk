//go:build !ios_extension && !js

// §18.5's bound value types for sdk.Sidecar: its settings, a call's one terminal result and that result's callback.
// Each crosses gobind, and as json the C ABI and js, so each field is a string, a bool, an int32 or int64 count or
// milliseconds, an *Id, []byte or an exportedList, never a sidecar-package value (R-SDK-5).

package sdk

import (
	"errors"
	"fmt"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
)

// A component the core attaches by name (§5.4).
const (
	SidecarComponentEvs     = "evs"
	SidecarComponentMail    = "mail"
	SidecarComponentSockets = "sockets"
	SidecarComponentBridge  = "bridge"
	SidecarComponentTls     = "tls"
)

// Every name SidecarSettings.Components may hold.
var sidecarComponentNames = []string{
	SidecarComponentEvs,
	SidecarComponentMail,
	SidecarComponentSockets,
	SidecarComponentBridge,
	SidecarComponentTls,
}

// What NewSidecarLocal takes. json in the C ABI and js.
type SidecarSettings struct {
	// The instance (§5.9): a lowercase letter or digit, then up to 31 of those and dashes. Empty is "default".
	Instance string `json:"instance,omitempty"`
	// The directory of the instance's sealed state, absolute: the app's no-backup files directory on Android, its
	// container on iOS. Required on the local backend; nothing in it is readable without the key store's wrap or
	// the pass.
	StateDir string `json:"state_dir,omitempty"`
	// The components the core attaches, by name: evs, mail, sockets, bridge and tls (§5.4). A name this sdk has not
	// bound yet is refused, never dropped.
	Components *StringList `json:"components,omitempty"`
	// The process's budget for this Sidecar, §6's caps and search memory inside it (§18.4): 0 takes the host class's,
	// and a negative value is refused.
	MemoryBudgetByteCount int64 `json:"memory_budget_byte_count,omitempty"`
	// §18.4's host class: ios, android or desktop. Empty is this build's platform's: ios and android on those, desktop
	// everywhere else.
	HostClass string `json:"host_class,omitempty"`
}

// The defaults: the default instance, no components, and the core's budget.
func DefaultSidecarSettings() *SidecarSettings {
	return &SidecarSettings{
		Instance:   "default",
		Components: NewStringList(),
	}
}

// One call's terminal result (§18.5; plan-SDK-small-readings #5). json in the C ABI and js.
type SidecarResult struct {
	// The call did what it asked.
	Ok bool `json:"ok"`
	// What the call names: a repo id, a send id, approval_pending:<id> while an outward action waits for a person's
	// decision, or authority_pending:<proposal_id> while other AdminSet devices' cosigns are due.
	Value string `json:"value,omitempty"`
	// A REVOKE_ or EVS_ code, for a refusal that has one.
	Code string `json:"code,omitempty"`
	// What went wrong, for a person. Never a token, a key or a secret (I-7).
	Message string `json:"message,omitempty"`
}

// Answers once, with a result or an error (sdk/api.go's callbacks).
type SidecarResultCallback connect.ApiCallback[*SidecarResult]

// The prefix of a result's Value while an outward action waits for a decision, before the approval's id (§18.5's
// gate). A format value: apps read it.
const sidecarApprovalPendingPrefix = "approval_pending:"

// A call's result from what the core answered: value on success; approval_pending:<id> when the core's gate
// (Sidecar.Authorize) holds the action for a person, so no wrapper adds a second check; a refusal's code where it
// has one. A pending action is not Ok: it has not happened.
func sidecarResultOf(value string, err error) *SidecarResult {
	if err == nil {
		return &SidecarResult{Ok: true, Value: value}
	}
	var pending *sidecar.ApprovalPendingError
	if errors.As(err, &pending) {
		return &SidecarResult{
			Value:   sidecarApprovalPendingPrefix + pending.ApprovalId.String(),
			Message: fmt.Sprintf("approval %s is pending", pending.ApprovalId),
		}
	}
	var revokeErr *sidecar.DeviceRevokeError
	if errors.As(err, &revokeErr) {
		return &SidecarResult{Code: revokeErr.Code, Message: revokeErr.Error()}
	}
	return &SidecarResult{Message: err.Error()}
}
