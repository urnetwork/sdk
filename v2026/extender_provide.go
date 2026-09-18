package sdk

import (
	"net"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// extender_provide.go — the provider extender surface (EXTENDER.md F3, G1).
//
// The role itself is compiled for desktop only (`device_local_extender_native.
// go`), so everything an app can call lives here, on every platform: the
// setting, the status and its change listener. A build that carries no role
// reports a disabled status rather than nothing, so a ui never has to branch
// on the platform.
//
// The setting is persisted per network space as `.provide_extender`, beside
// the other dot files, and defaults to on (G1).

// At most one status callback per second (F3), which is the epoch the space's
// extender status already uses.
const extenderProvideStatusEpoch = 1 * time.Second

// extenderProvideRoleEnabled is a process-wide switch for the provider
// extender role, exactly as the network client's and the member node's are.
// Production never changes it; the sdk test binary turns it off in TestMain so
// every test that turns providing on does not bind the carrier ports, and the
// provider extender tests turn it back on for their own device.
var extenderProvideRoleEnabled = true

// The states an app renders, derived once here so every app shows one rule
// (N3). Exported as plain strings, which is what gomobile, the cgo abi and the
// js types all carry; the apps map them to their own color and localized text.
const (
	// The setting is off.
	ExtenderProvideStateOff = "off"
	// The setting is on but the device is not providing.
	ExtenderProvideStateNotProviding = "not_providing"
	// The role runs and there is no outcome yet: the carriers are binding, or
	// the first activation is in flight.
	ExtenderProvideStateSettingUp = "setting_up"
	// At least one family is activated.
	ExtenderProvideStateActive = "active"
	// Revoked, no carrier bound, or the last activation failed or was refused
	// with no family active. `ErrorCase` names which, and `Reason` carries the
	// raw text.
	ExtenderProvideStateError = "error"
)

// The cases of the error state, so an app picks its label without re-deriving
// the rule (N3, N5). Set only while the state is error, empty in every other
// state.
const (
	// The operator revoked this extender's key. There is no reason text.
	ExtenderProvideErrorRevoked = "revoked"
	// The role was asked to run and could not start in this space.
	ExtenderProvideErrorStart = "start"
	// No carrier bound. The reason is the bind failures.
	ExtenderProvideErrorListen = "listen"
	// The last activation failed and no family is active.
	ExtenderProvideErrorActivationFailed = "activation_failed"
	// The operator refused the last activation and no family is active.
	ExtenderProvideErrorActivationRefused = "activation_refused"
)

// The provider extender state as the apps render it (F3). Every field is a
// gomobile-bindable value: the times are unix milliseconds, and the per-carrier
// bind failures are one string rather than a bound map.
type ExtenderProvideStatus struct {
	// True in a process built with the role (G1): a desktop or connectctl
	// binary. An ios, android or js build answers false, and so does a device
	// process too old to report the status at all, so an app hides the row
	// rather than drawing a dead toggle (N1, N2).
	Supported bool
	// The state an app renders, one of the ExtenderProvideState values above,
	// derived once from the fields below plus the setting and whether the
	// device is providing (N3).
	State string
	// Which error the state is, one of the ExtenderProvideError values above.
	// Empty in every state but error; in active, `Reason` alone carries the
	// other family's failure.
	ErrorCase string
	// The raw error text behind the state, empty when it has none. The app
	// prefixes the localized case label (N3, N5).
	Reason string
	// True while the role is running: this build carries it, the setting is on
	// and the device is providing.
	Enabled bool
	// Why the role could not start while it was asked to: the space has no
	// extender directory or no identity. Empty while the role runs or was not
	// asked to.
	StartError string
	// True while at least one carrier is bound.
	Listening bool
	// The carriers that failed to bind, as "<carrier>: <error>" joined by
	// "; ". Empty when every configured carrier bound.
	ListenError string
	ActivatedV4 bool
	ActivatedV6 bool
	// The addresses the operator published for this extender, empty when the
	// family is not activated.
	Ipv4 string
	Ipv6 string
	// Unix milliseconds of the last completed activation attempt of either
	// family, 0 when there has been none.
	LastActivationTime int64
	// Why the last attempt failed, empty when it succeeded.
	LastActivationError string
	// True when LastActivationError is the operator's refusal (an answer with
	// activated false) rather than a request that failed. It rides beside the
	// error in every state: in error it picks the refused case, and in active
	// it says whether the other family's text is a refusal (N2, N3).
	LastActivationRefused bool
	// Unix milliseconds of when this extender's key was first seen revoked in
	// the directory, 0 while it is not revoked.
	RevokedTime int64
	// The dns carrier ports that bound, ascending and comma separated, which
	// is also the order a client dials them in (L2): "4053" on a host that
	// cannot take 53, "53,4053" on one that can. Empty when the dns carrier is
	// not listening.
	DnsPorts string
	// Connections open over every carrier right now.
	ConnectionCount int
}

type ExtenderProvideStatusChangeListener interface {
	ExtenderProvideStatusChanged(status *ExtenderProvideStatus)
}

// ExtenderStats is the traffic the provider extender role has relayed, summed
// over every carrier and cumulative for the life of the role's server (O1,
// O2), the extender counterpart of a device's PacketStats. Operator-centric:
// ingress is what moved from a client toward the operator (into the network)
// and egress what moved back toward the client. A read is one chunk the relay
// moved on one side; a byte stream has no packet boundary in userspace, so the
// extender chart says reads where the provider chart says packets. The role
// that stops and starts again reports a fresh server's counters from zero.
// Every field is a gomobile-bindable value.
type ExtenderStats struct {
	IngressByteCount ByteCount
	IngressReadCount int64
	EgressByteCount  ByteCount
	EgressReadCount  int64
}

// The part of the status that changes on an event rather than continuously.
// Comparable, so it rides a MonitorValue and a watcher is woken only on an
// actual change; the connection count is deliberately outside it, since it
// moves with every relayed connection and no ui wants a callback per
// connection.
type extenderProvideState struct {
	Enabled               bool
	Listening             bool
	ListenError           string
	ActivatedV4           bool
	ActivatedV6           bool
	Ipv4                  string
	Ipv6                  string
	LastActivationTime    int64
	LastActivationError   string
	LastActivationRefused bool
	RevokedTime           int64
	DnsPorts              string
}

func (self extenderProvideState) status(connectionCount int) *ExtenderProvideStatus {
	return &ExtenderProvideStatus{
		Enabled:               self.Enabled,
		Listening:             self.Listening,
		ListenError:           self.ListenError,
		ActivatedV4:           self.ActivatedV4,
		ActivatedV6:           self.ActivatedV6,
		Ipv4:                  self.Ipv4,
		Ipv6:                  self.Ipv6,
		LastActivationTime:    self.LastActivationTime,
		LastActivationError:   self.LastActivationError,
		LastActivationRefused: self.LastActivationRefused,
		RevokedTime:           self.RevokedTime,
		DnsPorts:              self.DnsPorts,
		ConnectionCount:       connectionCount,
	}
}

// The status of a device that runs no role: this build has none, the setting
// is off, or the device is not providing. The state of N3 is stamped on by the
// device that reports it, which is the only thing that knows the setting.
func disabledExtenderProvideStatus() *ExtenderProvideStatus {
	return &ExtenderProvideStatus{}
}

// The status of a device that cannot describe the role at all (N2): a build
// that carries none, a device process out of contact, or one too old to answer
// the read. Off rather than nothing, so an app that draws the row anyway
// renders a state; N1 hides it while Supported is false.
func unsupportedExtenderProvideStatus() *ExtenderProvideStatus {
	return &ExtenderProvideStatus{State: ExtenderProvideStateOff}
}

// A copy of one status. Every field is a plain value, so the cached last value
// of the rpc reader is handed out as a copy rather than as the cache itself.
func cloneExtenderProvideStatus(status *ExtenderProvideStatus) *ExtenderProvideStatus {
	if status == nil {
		return nil
	}
	copied := *status
	return &copied
}

// The one rule every app renders (N3), derived from the role's own fields plus
// the two halves of the condition that starts it: the user's setting, and
// whether the device would run the role if the setting allowed it. Tested in
// this order, the first match winning:
//
//   - off: the setting is off (or this build carries no role at all).
//   - not_providing: the setting is on and the device is not providing --
//     provide mode none, the embedder's switch off, or a hosted device.
//   - error, start: the role was asked to run and could not start in this
//     space. A role that never started has no revocation, family or bind to
//     report, and an outcome exists, so it is not setting up.
//   - error, revoked: the operator revoked this extender's key. The case is
//     the whole message, so there is no reason text.
//   - active: at least one family is activated, with the other family's last
//     error beside it when that family failed, else nothing.
//   - error, listen: no carrier bound, and the bind failures say why.
//   - error, activation: the last activation failed or was refused and no
//     family is active. This stands through the activator's backoff, since an
//     outcome exists; yellow is only ever the time before the first outcome.
//   - setting_up: anything else, which is the role running with no outcome
//     yet -- the carriers binding, or the first activation in flight.
//
// It answers the state, the error case (empty outside the error state) and
// the reason. Pure: everything it reads is an argument, so the table test of
// N6 is the whole rule.
func extenderProvideStateRule(
	status *ExtenderProvideStatus,
	provideExtender bool,
	providing bool,
) (state string, errorCase string, reason string) {
	switch {
	case status == nil || !status.Supported:
		// this process carries no role, so there is nothing to be setting up
		return ExtenderProvideStateOff, "", ""
	case !provideExtender:
		return ExtenderProvideStateOff, "", ""
	case !providing:
		return ExtenderProvideStateNotProviding, "", ""
	case !status.Enabled && status.StartError != "":
		return ExtenderProvideStateError, ExtenderProvideErrorStart, status.StartError
	case status.RevokedTime != 0:
		return ExtenderProvideStateError, ExtenderProvideErrorRevoked, ""
	case status.ActivatedV4 || status.ActivatedV6:
		// a family that succeeds clears its own error, so an error that still
		// stands here belongs to the family that did not activate (F3)
		return ExtenderProvideStateActive, "", status.LastActivationError
	case !status.Listening && status.ListenError != "":
		// a role whose carriers are still binding is not listening either, but
		// has no failure yet, and falls through to setting_up
		return ExtenderProvideStateError, ExtenderProvideErrorListen, status.ListenError
	case status.LastActivationTime != 0 && status.LastActivationError != "":
		return ExtenderProvideStateError,
			extenderProvideActivationErrorCase(status),
			status.LastActivationError
	default:
		return ExtenderProvideStateSettingUp, "", ""
	}
}

// Whether a standing activation error is the operator's refusal or a failed
// request (N3, N5): the activator records which on the family whose error the
// status reports, and this is the one place the rule reads it.
func extenderProvideActivationErrorCase(status *ExtenderProvideStatus) string {
	if status.LastActivationRefused {
		return ExtenderProvideErrorActivationRefused
	}
	return ExtenderProvideErrorActivationFailed
}

// The status as an app reads it: this build's Supported, and the state of N3
// derived from the setting and the providing state. Applied by whatever
// reports the status, since the role itself knows neither.
func (self *ExtenderProvideStatus) withState(
	provideExtender bool,
	providing bool,
) *ExtenderProvideStatus {
	self.Supported = extenderProvideSupported
	self.State, self.ErrorCase, self.Reason = extenderProvideStateRule(
		self,
		provideExtender,
		providing,
	)
	return self
}

// What one extender role is built from (G2, G3). The provider fills it from
// the network space and the device's own egress; a test replaces the ports,
// the listen seams and the operator urls through
// `DeviceLocalSettings.providerExtenderSettings`.
type deviceLocalExtenderSettings struct {
	Log connect.Logger

	// The space this extender belongs to. Its directory holds the records, its
	// node becomes the extender node, and its local state holds the identity
	// key (B1, G2).
	NetworkSpace *NetworkSpace

	// The operator patterns this extender may forward to (A5): `<host>` and
	// `*.<host>` of the space host and the migration host.
	AllowedHosts []string
	// SpoofDomains, when set, replaces the bundled list in the whitelist (A5,
	// A10). Nil takes connect.SpoofDomains(), which is the production list.
	SpoofDomains []string

	// The identity this extender is activated under (B1), the space's
	// persisted `.extender_key` seed.
	IdentityKeySeed []byte

	// The carrier ports (A1). Tests bind ephemeral ports through them.
	TcpPort int
	UdpPort int
	DnsPort int
	DnsTld  string
	// DnsPrivilegedPort also binds the dns carrier on 53 beside DnsPort (L2).
	// The provider fills it from the platform rule; a test pins it off so its
	// ephemeral carrier is the only dns port on any host.
	DnsPrivilegedPort bool

	// The family api urls the activation is posted to, and the plain api url
	// for an operator that has neither (C2, G3).
	ApiUrlV4 string
	ApiUrlV6 string
	ApiUrl   string
	// The api url whose `/hello` reports the caller address this extender is
	// published under (G3).
	HelloUrl string
	// The client jwt, read at each activation so a refresh is picked up.
	ByJwt func() string

	// The strategy configuration every activation is posted through. The role
	// builds a direct-only strategy from it: an activation that crossed an
	// extender would tell the operator that extender's address (C2).
	ClientStrategySettings *connect.ClientStrategySettings
	// The forward dial of the relay (G2): the device's egress-aware dial, so
	// the relay never enters the device's own tunnel. The extender narrows it
	// by the client's family itself (A7).
	DialContext connect.DialContextFunction

	// Listen and ListenPacket, when set, bind the carriers. Tests bind
	// ephemeral loopback sockets through them.
	Listen       func(network string, address string) (net.Listener, error)
	ListenPacket func(network string, address string) (net.PacketConn, error)

	// The activation cadences (G3). Zero takes the design defaults.
	ActivateTimeout     time.Duration
	AddressCheckTimeout time.Duration
	MinBackoff          time.Duration
	MaxBackoff          time.Duration
	RequestTimeout      time.Duration

	// Now and IpVersionSupported, when set, replace the clock and the host
	// family probe of the activation loop. Tests pin both.
	Now                func() time.Time
	IpVersionSupported func(ipVersion int) bool
}

// Whether the dns carrier also binds 53 beside its unprivileged port (L2).
// Only the platforms that can take 53 without privilege do: the linux daemon
// runs as root and the windows service as LocalSystem, while macOS and every
// other host binds 4053 alone. The bind is never required -- a failure
// disables that one port and the carrier keeps serving.
func extenderDnsPrivilegedPort() bool {
	return extenderDnsPrivilegedPortForPlatform(runtime.GOOS)
}

// The rule itself, parameterized by the platform so it is pinned by a test on
// any host.
func extenderDnsPrivilegedPortForPlatform(goos string) bool {
	switch goos {
	case "linux", "windows":
		return true
	default:
		return false
	}
}

// The bound dns ports of one extender as one string (F3, L2), ascending, which
// is the order a client dials them in. Empty when the dns carrier is not
// listening.
func extenderDnsPortsText(dnsPorts []int) string {
	parts := []string{}
	for _, dnsPort := range dnsPorts {
		parts = append(parts, strconv.Itoa(dnsPort))
	}
	return strings.Join(parts, ",")
}

// The bind failures of one extender as one string (F3), in carrier order so
// the text is stable across reads.
func extenderListenErrorText(carrierErrs map[string]error) string {
	if len(carrierErrs) == 0 {
		return ""
	}
	parts := []string{}
	for _, carrier := range []string{
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
	} {
		if err, ok := carrierErrs[carrier]; ok && err != nil {
			parts = append(parts, carrier+": "+err.Error())
		}
	}
	return strings.Join(parts, "; ")
}

// The provider extender setting of this device's space (F3). A device with no
// storage always reads the default, which is on.
func (self *DeviceLocal) GetProvideExtender() bool {
	asyncLocalState := self.networkSpaceAsyncLocalState()
	if asyncLocalState == nil {
		return true
	}
	return asyncLocalState.GetLocalState().GetProvideExtender()
}

// Persists the provider extender setting and applies it at once (F3, G2).
// Turning it off stops the extender server, the activation loop and the
// extender node together; turning it on starts them again while the device is
// providing. A hosted device never runs the role (G1), so the setter is
// guarded there as SetProvideMode is.
func (self *DeviceLocal) SetProvideExtender(provideExtender bool) {
	if self.hostedIncompatibleGuarded("SetProvideExtender") {
		return
	}
	if asyncLocalState := self.networkSpaceAsyncLocalState(); asyncLocalState != nil {
		if err := asyncLocalState.GetLocalState().SetProvideExtender(provideExtender); err != nil {
			self.log.Infof("[device]provide extender err = %s\n", err)
		}
	}
	self.log.Infof("[device]provide extender = %t\n", provideExtender)
	self.updateExtenderProvide()
}

// The provider extender status (F3). A device that runs no role reports a
// disabled status rather than nil, so a caller never has to branch. The state
// and reason of N3 are derived here, so every consumer -- this device, the rpc
// and every app behind it -- renders the same rule (N2).
func (self *DeviceLocal) GetExtenderProvideStatus() *ExtenderProvideStatus {
	self.stateLock.Lock()
	provider := self.provider
	closed := self.closed
	self.stateLock.Unlock()
	status := disabledExtenderProvideStatus()
	if !closed && provider != nil {
		status = provider.extenderProvideStatus()
	}
	return status.withState(self.GetProvideExtender(), self.extenderProvideProviding())
}

// The relayed traffic of the provider extender role (O2), nil whenever the
// role is not running: this build carries none, the setting is off, the device
// is not providing (or is hosted, or the embedder's switch is off), the role
// could not start in this space (the status carries why), or the device is
// closed. Nil is what tells an app there is no series to show; the role's
// status reports the same fact as Enabled.
func (self *DeviceLocal) GetExtenderStats() *ExtenderStats {
	self.stateLock.Lock()
	provider := self.provider
	closed := self.closed
	self.stateLock.Unlock()
	if closed || provider == nil {
		return nil
	}
	return provider.extenderStats()
}

// Whether this device would run the role if the setting allowed it (G1, G2):
// it is providing, the embedder's switch is on, and it is not hosted -- a
// hosted device's space is shared across unrelated customers and it cannot
// provide at all. The setting is the other half of the same condition, and
// N3's first two states are exactly these two halves.
func (self *DeviceLocal) extenderProvideProviding() bool {
	self.stateLock.Lock()
	closed := self.closed
	self.stateLock.Unlock()
	return !closed &&
		!self.settings.HostedIncompatible &&
		self.settings.ProvideExtenderEnabled &&
		self.GetProvideEnabled()
}

// AddExtenderProvideStatusChangeListener subscribes to the provider extender
// status. The callbacks are coalesced to at most one per second by the watch
// below, which is what keeps a burst of activation and directory changes from
// becoming a burst of ui work (F3).
func (self *DeviceLocal) AddExtenderProvideStatusChangeListener(
	listener ExtenderProvideStatusChangeListener,
) Sub {
	callbackId := self.extenderProvideStatusChangeListeners.Add(listener)
	return newSub(func() {
		self.extenderProvideStatusChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) extenderProvideStatusChanged() {
	status := self.GetExtenderProvideStatus()
	for _, listener := range self.extenderProvideStatusChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ExtenderProvideStatusChanged(status)
		})
	}
}

// Starts or stops the extender role for the current provide state and setting
// (G2). Called after every provide change and after the setting changes, never
// with the device lock held.
func (self *DeviceLocal) updateExtenderProvide() {
	self.stateLock.Lock()
	provider := self.provider
	self.stateLock.Unlock()
	if provider != nil {
		// the embedder's switch and the user's setting must both allow it (G1,
		// F3). A hosted device never runs it: its space is shared across
		// unrelated customers, and an extender published for this host would
		// name the proxy host's own address. That device cannot provide
		// either, so this is defense in depth beside the hosted provide guard.
		// The same two halves are what N3's off and not_providing states
		// report.
		provider.setExtenderEnabled(self.extenderProvideProviding() && self.GetProvideExtender())
	}
	// the watch is woken whether or not there is a provider: the state of N3
	// follows the setting and the provide state, so a device with no provider
	// still moves between off and not_providing (N2, N3)
	self.extenderProvideMonitor.NotifyAll()
}

// watchExtenderProvideStatus emits at most one status per epoch, carrying the
// complete state (F3).
//
// It waits for a change, sleeps the epoch so a burst collapses, and only then
// re-arms and reads. Arming before the epoch would leave the next round
// already triggered by the same burst the snapshot already carries. See
// NetworkSpace.watchExtenderStatus for the same shape.
//
// deviceUpdate is the device's wake, armed by the constructor before this
// goroutine starts. Armed here instead, a change landing before the goroutine's
// first statement would close a channel nobody held yet, and a device with no
// role, which nothing else wakes, would never push it (N2).
func (self *DeviceLocal) watchExtenderProvideStatus(deviceUpdate chan struct{}) {
	roleUpdate := self.extenderProvideStatusUpdate()
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-deviceUpdate:
		case <-roleUpdate:
		}
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(extenderProvideStatusEpoch):
		}
		deviceUpdate = self.extenderProvideMonitor.NotifyChannel()
		roleUpdate = self.extenderProvideStatusUpdate()
		// contain a panic to the tick: a failed emit must never end the watch,
		// which would silently stop every provider extender update
		connect.HandleError(self.extenderProvideStatusChanged)
	}
}

// A channel armed at the instant of the read, so the watch is woken when the
// running role's status changes. A device with no role waits on nil, which
// never fires; the device's own monitor carries the start.
func (self *DeviceLocal) extenderProvideStatusUpdate() chan struct{} {
	self.stateLock.Lock()
	provider := self.provider
	self.stateLock.Unlock()
	if provider == nil {
		return nil
	}
	return provider.extenderStatusUpdate()
}

// The local state of this device's space, nil when the space keeps none.
func (self *DeviceLocal) networkSpaceAsyncLocalState() *AsyncLocalState {
	if self.networkSpace == nil {
		return nil
	}
	return self.networkSpace.asyncLocalState
}
