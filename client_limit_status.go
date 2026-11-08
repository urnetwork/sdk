package sdk

// client_limit_status.go -- the client limit status of the device's platform
// connection, for apps to show while the platform holds the device off, and
// what an app offers for a client limit refusal.
//
// The platform closes a client over its network's concurrent client limit
// with the client limit close (connect/transport_client_limit.go): a device
// that declared provide intent and did not qualify as a public provider while
// no normal client slot is free, or an ordinary client over the limit. The
// device then holds every platform transport of its client for at least 15
// minutes instead of reconnecting, and reports ClientLimitStatusExceeded with
// the time the hold ends. The status returns to ClientLimitStatusNone when the
// hold ends; a new close sets it again. The platform tells the client nothing
// else about provider qualification.
//
// A device declares provide intent by itself while its provide mode includes
// public (see deviceLocalProvider.setProvideMode); an app has nothing to set.
//
// The prompt is the decision an app shows, the same on every platform, for a
// hold (ClientLimitStatus.Prompt) and for a refused client creation at sign-in
// (AuthNetworkClientError.ClientLimitPrompt):
//   - ClientLimitPromptUpgrade: the network is on the free plan, and UR Pro lifts
//     the limit. The app offers its upgrade flow.
//   - ClientLimitPromptRemoveClient: no upgrade lifts the limit: a Pro network at
//     the Pro limit, or a network whose limit is an Embed allowance. The app says
//     to free a slot by signing out of a device the user no longer uses, and
//     offers no upgrade.
//
// The server decides which. At sign-in its refusal says so (upgrade_required).
// A hold's close says nothing, so the prompt of a hold follows the claims of the
// device's client jwt, which the device refreshes when a hold starts: the
// upgrade only when the client_limit_upgrade claim says UR Pro raises the
// network's limit. A jwt without the claim, from a server before it, is one the
// client cannot tell by, and gets no upgrade prompt, since an upgrade may not
// help (an Embed allowance).
//
// After the user acts on a hold's prompt, the app calls Device.RetryClientLimit
// to dial again at once instead of waiting out the hold. While the app is not
// in front, a local notification says the device is held, posted as
// ClientLimitNoticeTracker decides.

import (
	"github.com/urnetwork/connect"
)

const (
	// Keep these untyped so gomobile exports them on every app platform.

	// no client limit condition
	ClientLimitStatusNone = ""
	// the platform closed the device for its network's client limit, and the
	// device holds off reconnecting until RetryTime
	ClientLimitStatusExceeded = "client_limit_exceeded"
)

const (
	// Keep these untyped so gomobile exports them on every app platform.

	// nothing to offer: no client limit refusal
	ClientLimitPromptNone = ""
	// UR Pro lifts the limit: offer the upgrade flow
	ClientLimitPromptUpgrade = "upgrade"
	// no plan lifts the limit: say how to free a slot, with no upgrade
	ClientLimitPromptRemoveClient = "remove_client"
)

// ClientLimitStatus is one readout of the device's client limit hold. Plain
// string and int64 fields so gomobile and the cgo/js bindings carry it as is.
type ClientLimitStatus struct {
	// ClientLimitStatusNone or ClientLimitStatusExceeded.
	Status string
	// When the hold ends and the device reconnects, in unix epoch
	// milliseconds. 0 while Status is ClientLimitStatusNone.
	RetryTime int64
	// What the app offers while Status is ClientLimitStatusExceeded:
	// ClientLimitPromptUpgrade or ClientLimitPromptRemoveClient, from the
	// server's claims in the device's client jwt (refreshed when a hold
	// starts): the upgrade only when the server says UR Pro raises the
	// network's limit, which it never does for a Pro network or an Embed
	// allowance. ClientLimitPromptNone while Status is ClientLimitStatusNone.
	Prompt string
}

// ClientLimitStatusChangeListener receives every change of the client limit
// status (see Device.GetClientLimitStatus).
type ClientLimitStatusChangeListener interface {
	ClientLimitStatusChanged(status *ClientLimitStatus)
}

// clientLimitUpgrade is the client_limit_upgrade claim of a client jwt.
type clientLimitUpgrade int8

const (
	// the jwt has no claim: a server before it
	clientLimitUpgradeUnknown clientLimitUpgrade = iota
	// UR Pro raises the network's concurrent client limit: a free network
	// without an Embed allowance
	clientLimitUpgradeRaises
	// no upgrade raises it: a Pro network, or an Embed allowance
	clientLimitUpgradeNone
)

// clientLimitPlan is what the prompt of a hold follows: the plan claims of the
// device's client jwt.
type clientLimitPlan struct {
	// the pro claim
	pro bool
	// the client_limit_upgrade claim
	upgrade clientLimitUpgrade
}

// newClientLimitStatus maps a connect hold readout to the app form, with the
// prompt of the network's plan.
func newClientLimitStatus(status connect.ClientLimitStatus, plan clientLimitPlan) *ClientLimitStatus {
	if !status.Exceeded {
		return noneClientLimitStatus()
	}
	return &ClientLimitStatus{
		Status:    ClientLimitStatusExceeded,
		RetryTime: status.RetryTime.UnixMilli(),
		Prompt:    clientLimitPromptForPlan(plan),
	}
}

// clientLimitPromptForPlan is the prompt of a network over its concurrent
// client limit: UR Pro when the server says it raises the limit (a free
// network without an Embed allowance); otherwise a freed slot: a Pro network
// is already on the highest plan, no plan lifts an Embed allowance, and a jwt
// without the server's claim cannot tell which.
func clientLimitPromptForPlan(plan clientLimitPlan) string {
	if plan.upgrade == clientLimitUpgradeRaises && !plan.pro {
		return ClientLimitPromptUpgrade
	}
	return ClientLimitPromptRemoveClient
}

// clientLimitPromptForRefusal is the prompt of a refused auth-client: an upgrade
// when the server says one lifts the refusal (upgrade_required, which a Pro-only
// feature also sets and the same upgrade flow lifts), a free slot when it is a
// client limit no upgrade lifts (client_limit_exceeded alone: a Pro network at
// the Pro limit, an Embed plan's allowance, the top-level client cap), and
// nothing for any other refusal.
func clientLimitPromptForRefusal(refusal *AuthNetworkClientError) string {
	switch {
	case refusal == nil:
		return ClientLimitPromptNone
	case refusal.UpgradeRequired:
		return ClientLimitPromptUpgrade
	case refusal.ClientLimitExceeded:
		return ClientLimitPromptRemoveClient
	default:
		return ClientLimitPromptNone
	}
}

const (
	// Keep these untyped so gomobile exports them on every app platform.

	// leave the notice as it is
	ClientLimitNoticeNone = ""
	// show the notice and alert the user
	ClientLimitNoticePost = "post"
	// show the notice, or replace the one showing, without alerting
	ClientLimitNoticeUpdate = "update"
	// withdraw the notice
	ClientLimitNoticeRemove = "remove"
)

// ClientLimitNoticeTracker decides the local notification an app posts for the
// device's client limit hold, the same on every platform; the app supplies only
// the words and the notification itself. The notice shows while a hold is in
// force and a connection is requested, since the user's traffic then goes
// nowhere until the user acts, and it says the hold's prompt. It alerts once
// per connection request: a hold that returns because the platform refused the
// device again at its retry time shows the notice again without alerting, and a
// prompt change replaces it in place. The notice goes when the hold ends, when
// the connection request ends, and at Reset (sign out).
//
// Not safe for concurrent use: an app calls it from one thread, its main thread.
type ClientLimitNoticeTracker struct {
	// a notice shows, saying shownPrompt
	shown       bool
	shownPrompt string
	// a notice alerted during this connection request
	alerted bool
}

func NewClientLimitNoticeTracker() *ClientLimitNoticeTracker {
	return &ClientLimitNoticeTracker{}
}

// Update takes the device's current client limit status and whether a
// connection is requested, and returns what to do with the notice: one of the
// ClientLimitNotice actions.
func (self *ClientLimitNoticeTracker) Update(status *ClientLimitStatus, connectRequested bool) string {
	if !connectRequested {
		// the next request alerts again
		self.alerted = false
		return self.withdraw()
	}
	if status == nil || status.Status != ClientLimitStatusExceeded {
		return self.withdraw()
	}
	if self.shown && self.shownPrompt == status.Prompt {
		return ClientLimitNoticeNone
	}
	self.shown = true
	self.shownPrompt = status.Prompt
	if self.alerted {
		return ClientLimitNoticeUpdate
	}
	self.alerted = true
	return ClientLimitNoticePost
}

// Reset ends the connection request, for a sign out or an account switch, and
// returns what to do with the notice: ClientLimitNoticeRemove when one shows.
func (self *ClientLimitNoticeTracker) Reset() string {
	self.alerted = false
	return self.withdraw()
}

func (self *ClientLimitNoticeTracker) withdraw() string {
	if !self.shown {
		return ClientLimitNoticeNone
	}
	self.shown = false
	self.shownPrompt = ClientLimitPromptNone
	return ClientLimitNoticeRemove
}

// noneClientLimitStatus is the readout with no hold, and of a device with no
// platform connection to hold.
func noneClientLimitStatus() *ClientLimitStatus {
	return &ClientLimitStatus{
		Status: ClientLimitStatusNone,
	}
}

func cloneClientLimitStatus(status *ClientLimitStatus) *ClientLimitStatus {
	if status == nil {
		return nil
	}
	copied := *status
	return &copied
}
