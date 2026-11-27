//go:build js && !ios_extension

// §18.5's bound value types for the js leg, where the local backend's files, which declare them beside code that
// links the sidecar root, do not build (plan-SDK-small-readings #7): the settings, a call's result and its callback,
// the key store and presence signer an app implements, and the event with its listener. Each declaration here is the
// other legs' own, repeated word for word from sidecar_types.go, sidecar_keystore.go and sidecar_events.go;
// TestJsBoundTypesMatch (sidecar_remote_test.go) fails when one drifts. Lifting them into one file every leg builds
// is the review phase's.

package sdk

import (
	"github.com/urnetwork/connect"
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
	// The remote backend alone: the directory that holds the daemon's socket where the instance's own (§5.9) is out of
	// reach, which is a sandboxed macOS app's app-group container (§5.8's Pairing). Absolute. Empty dials §5.9's
	// socket path for the instance.
	DaemonSocketDir string `json:"daemon_socket_dir,omitempty"`
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

// What PutSecret and DeleteSecret answer: the core's SidecarKeyStoreStatus numbers, one for one. Zero is Failed, so a
// store that sets nothing is never read as anything else.
const (
	// A fault, or a value the store cannot vouch for. Nothing is created, overwritten or deleted on it.
	SidecarKeyStoreFailed int32 = 0
	// The write took: kept, or deleted or absent.
	SidecarKeyStoreOk int32 = 1
	// The store cannot answer until it unlocks, after which the app calls Unlock(""). Nothing is created,
	// overwritten or deleted on it.
	SidecarKeyStoreLocked int32 = 4
)

// What a SidecarSecret's Status says: the core's numbers, one for one, zero Failed.
const (
	// A fault, or a value the store cannot vouch for.
	SidecarSecretFailed int32 = 0
	// The value is in ValueBase64.
	SidecarSecretPresent int32 = 2
	// The store works and holds no such item, which a lock must never be reported as.
	SidecarSecretAbsent int32 = 3
	// The store cannot answer until it unlocks.
	SidecarSecretLocked int32 = 4
)

// The app's store of the sidecar's secrets, which never throws across the binding: each call answers once, with a
// status. Called on Go threads, possibly several at once.
type SidecarKeyStore interface {
	// Keeps the value under name, replacing any earlier one: SidecarKeyStoreOk, SidecarKeyStoreLocked or
	// SidecarKeyStoreFailed.
	PutSecret(name string, valueBase64 string) int32
	// The value under name, with its status. Never nil.
	GetSecret(name string) *SidecarSecret
	// SidecarKeyStoreOk whether it was deleted or absent, else SidecarKeyStoreLocked or SidecarKeyStoreFailed.
	DeleteSecret(name string) int32
}

// GetSecret's answer. json in the C ABI and js.
type SidecarSecret struct {
	// SidecarSecretPresent, SidecarSecretAbsent, SidecarSecretLocked or SidecarSecretFailed.
	Status int32 `json:"status"`
	// Standard base64 of the value, set only when Present.
	ValueBase64 string `json:"value_base64,omitempty"`
}

// The app's presence key: a Secure Enclave or platform FIDO key, per §16.6's profiles. Nil: another device approves.
// SidecarApprovals' decisions and revocations sign through it wherever the log covers their act (sidecar_presence.go).
type SidecarPresenceSigner interface {
	// The public key and its attestation, registered per §16.6.
	GetAttestation() string
	// After a presence check with user verification over the message (webauthn-es256: its SHA-256 as the
	// challenge), base64 of §6.9's WebAuthnEvidence; "" if refused.
	SignWithPresence(messageBase64 string) string
}

// One event. A field is set only for the kinds its comment names; the rest are zero. json in the C ABI and js.
type SidecarEvent struct {
	// The core's kind: state, approval_pending, member_added, member_removed, key_changed, refused, admission_alarm,
	// recovery_rebind, recovery_window, recovery_unseen, admissions_near_cap, head_alarm, index_mismatch,
	// index_malformed, search_alarm, history_drop, access_request, access_decided, directory_erasure, erasure,
	// mail_send, evs_conflict, evs_alarm, evs_gap, credential_store, record_alarm, bridge_alarm, dns_alarm, ct_alarm,
	// mail_receiver_alarm, and any a later component adds.
	Kind string `json:"kind"`
	// The client generation it comes from (§18.5's State block).
	Generation int64 `json:"generation"`
	// state: locked, unenrolled, offline, online or revoked.
	State string `json:"state,omitempty"`
	// state: the reason of an unenrolled or revoked state.
	Reason string `json:"reason,omitempty"`
	// member_added, member_removed: the group.
	GroupId *Id `json:"group_id,omitempty"`
	// The sidecar its kind names: a group's member, an admitted, recovered or rebinding device, key_changed's sidecar,
	// a head alarm's peer, an index alarm's or drop's signer, a search alarm's indexer, an evs_alarm's or
	// record_alarm's signer where one is known, a ct_alarm's attesting identity, a mail_receiver_alarm's receiver or
	// record signer.
	SidecarId *Id `json:"sidecar_id,omitempty"`
	// Index alarms, search_alarm, history_drop, directory_erasure, erasure, the evs kinds, record_alarm's log and
	// mail_receiver_alarm.
	RepoId *Id `json:"repo_id,omitempty"`
	// Index alarms, history_drop and the evs kinds.
	RefId *Id `json:"ref_id,omitempty"`
	// Index alarms: the path; history_drop: its first listed path; evs_conflict: the local conflict ref; evs_alarm:
	// the path, where one is named. Local to this device: never log it (I-13).
	Path string `json:"path,omitempty"`
	// history_drop.
	DropId *Id `json:"drop_id,omitempty"`
	// history_drop: pending, cancelled, executing, done, refused or invalid.
	DropState string `json:"drop_state,omitempty"`
	// history_drop: its window's end, in unix milliseconds; 0 for none.
	NotBeforeMillis int64 `json:"not_before_millis,omitempty"`
	// search_alarm: mismatch or omission.
	SearchAlarm string `json:"search_alarm,omitempty"`
	// access_request, access_decided, directory_erasure.
	RequestId *Id `json:"request_id,omitempty"`
	// access_decided: approved, denied, cancelled or expired; directory_erasure: pending or done.
	Outcome string `json:"outcome,omitempty"`
	// mail_send: the send, whose outcomes GetSendResult reads.
	SendId *Id `json:"send_id,omitempty"`
	// approval_pending.
	ApprovalId *Id `json:"approval_id,omitempty"`
	// head_alarm: fork, withheld, unverified or unwitnessed (§6.4), or rollback, a push reset that presented a durable
	// topic below the version this device holds (§6.8; owner decision 300).
	HeadAlarm string `json:"head_alarm,omitempty"`
	// head_alarm: this device's pin, or for rollback the TopicVersion held; recovery_rebind at a void: the AuthPin
	// keychain.wrap was sealed at; key_changed: the SHA-256 of the foreign key gkm served, where it names one.
	FirstEvidence []byte `json:"first_evidence,omitempty"`
	// head_alarm: the peer's or api's pin, or for rollback the TopicVersion offered; recovery_rebind at a void: the
	// AuthPin it was re-sealed at.
	SecondEvidence []byte `json:"second_evidence,omitempty"`
	// erasure.
	Erasure *SidecarErasure `json:"erasure,omitempty"`
	// evs_alarm: missing_segment, tamper, version_rule, epoch_overdue or prefix_rule.
	EvsAlarm string `json:"evs_alarm,omitempty"`
	// evs_alarm of reason prefix_rule: ignored, refused, set_void or set_newer, what it stands for.
	EvsAlarmDetail string `json:"evs_alarm_detail,omitempty"`
	// evs_alarm of reason prefix_rule: the ref version of the first change it names, or a set's log position.
	Version int64 `json:"version,omitempty"`
	// evs_alarm of reason prefix_rule: the push of the first change it names; nil for a set's.
	PushId *Id `json:"push_id,omitempty"`
	// evs_alarm of reason prefix_rule: how many changes it stands for.
	Count int64 `json:"count,omitempty"`
	// evs_gap: the newest re-pin past the catch-up budget, in unix milliseconds; 0 for none.
	VerifiedSinceMillis int64 `json:"verified_since_millis,omitempty"`
	// record_alarm: what the record found, one of the core's closed set (chain_gap, chain_fork, bad_entry, ...).
	RecordAlarm string `json:"record_alarm,omitempty"`
	// bridge_alarm: what the bridge found (block_conflict, state_unverified, map_refused, ...).
	BridgeAlarm string `json:"bridge_alarm,omitempty"`
	// dns_alarm: what the zone watch found (split, unrequested, zone_mismatch, ...).
	DnsAlarm string `json:"dns_alarm,omitempty"`
	// ct_alarm: what the CT watch found (unattested, recent_identity).
	CtAlarm string `json:"ct_alarm,omitempty"`
	// mail_receiver_alarm: why a reader alarmed (D-MAIL-15): undesignated, internet mail taken by a receiver that is
	// none of the network's mail indexers; scope, a member scope's deposit from a receiver that is no consented
	// device of the scope's member; deposit, a deposit ingest passed over or wrote as evidence unavailable; record, a
	// mailcfg version a reader passed over.
	MailReceiverAlarm string `json:"mail_receiver_alarm,omitempty"`
	// upgrade_required: the authorization features the network turned on that this release lacks, by name, comma
	// separated: the sidecar holds until the update that ends it (DESIGN-COMPUTE §16.6's hold).
	AuthFeatures string `json:"auth_features,omitempty"`
	// disclosure_changed: the disclosure whose acknowledgment for the network names an older version than Version, its
	// current one (DESIGN-COMPUTE §2.8).
	DisclosureId string `json:"disclosure_id,omitempty"`
}

// An erasure as the device running it, or one that may finish it, sees it (§11.12, §11.13's wait). json in the C ABI
// and js.
type SidecarErasure struct {
	ErasureId *Id `json:"erasure_id,omitempty"`
	// fenced, rewriting, waiting, done or stalled.
	State string `json:"state"`
	// Rekey objects not yet re-keyed.
	ObjectsLeft int64 `json:"objects_left,omitempty"`
	// Their closure bytes.
	BytesLeft int64 `json:"bytes_left,omitempty"`
	// The last 429's retry, in unix milliseconds; 0 when none waits.
	NextTryMillis int64 `json:"next_try_millis,omitempty"`
	// The bound on its finish, in unix milliseconds; 0 below the limit.
	FinishMillis int64 `json:"finish_millis,omitempty"`
}

// App-implemented: called on a Go thread with each event, in order.
type SidecarEventListener interface {
	SidecarEventReceived(event *SidecarEvent)
}
