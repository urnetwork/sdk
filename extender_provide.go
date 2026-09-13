package sdk

import (
	"net"
	"strings"
	"time"

	"github.com/urnetwork/connect"
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

// The provider extender state as the apps render it (F3). Every field is a
// gomobile-bindable value: the times are unix milliseconds, and the per-carrier
// bind failures are one string rather than a bound map.
type ExtenderProvideStatus struct {
	// True while the role is running: this build carries it, the setting is on
	// and the device is providing.
	Enabled bool
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
	// Unix milliseconds of when this extender's key was first seen revoked in
	// the directory, 0 while it is not revoked.
	RevokedTime int64
	// Connections open over every carrier right now.
	ConnectionCount int
}

type ExtenderProvideStatusChangeListener interface {
	ExtenderProvideStatusChanged(status *ExtenderProvideStatus)
}

// The part of the status that changes on an event rather than continuously.
// Comparable, so it rides a MonitorValue and a watcher is woken only on an
// actual change; the connection count is deliberately outside it, since it
// moves with every relayed connection and no ui wants a callback per
// connection.
type extenderProvideState struct {
	Enabled             bool
	Listening           bool
	ListenError         string
	ActivatedV4         bool
	ActivatedV6         bool
	Ipv4                string
	Ipv6                string
	LastActivationTime  int64
	LastActivationError string
	RevokedTime         int64
}

func (self extenderProvideState) status(connectionCount int) *ExtenderProvideStatus {
	return &ExtenderProvideStatus{
		Enabled:             self.Enabled,
		Listening:           self.Listening,
		ListenError:         self.ListenError,
		ActivatedV4:         self.ActivatedV4,
		ActivatedV6:         self.ActivatedV6,
		Ipv4:                self.Ipv4,
		Ipv6:                self.Ipv6,
		LastActivationTime:  self.LastActivationTime,
		LastActivationError: self.LastActivationError,
		RevokedTime:         self.RevokedTime,
		ConnectionCount:     connectionCount,
	}
}

// The status of a device that runs no role: this build has none, the setting
// is off, or the device is not providing.
func disabledExtenderProvideStatus() *ExtenderProvideStatus {
	return &ExtenderProvideStatus{}
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
// providing.
func (self *DeviceLocal) SetProvideExtender(provideExtender bool) {
	if asyncLocalState := self.networkSpaceAsyncLocalState(); asyncLocalState != nil {
		if err := asyncLocalState.GetLocalState().SetProvideExtender(provideExtender); err != nil {
			self.log.Infof("[device]provide extender err = %s\n", err)
		}
	}
	self.log.Infof("[device]provide extender = %t\n", provideExtender)
	self.updateExtenderProvide()
}

// The provider extender status (F3). A device that runs no role reports a
// disabled status rather than nil, so a caller never has to branch.
func (self *DeviceLocal) GetExtenderProvideStatus() *ExtenderProvideStatus {
	self.stateLock.Lock()
	provider := self.provider
	closed := self.closed
	self.stateLock.Unlock()
	if closed || provider == nil {
		return disabledExtenderProvideStatus()
	}
	return provider.extenderProvideStatus()
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
	closed := self.closed
	self.stateLock.Unlock()
	if provider == nil {
		return
	}
	// the embedder's switch and the user's setting must both allow it (G1,
	// F3). A hosted device never runs it: its space is shared across unrelated
	// customers, and an extender published for this host would name the proxy
	// host's own address. That device cannot provide either, so this is
	// defense in depth beside the hosted provide guard.
	provider.setExtenderEnabled(
		!closed &&
			!self.settings.HostedIncompatible &&
			self.settings.ProvideExtenderEnabled &&
			self.GetProvideEnabled() &&
			self.GetProvideExtender())
	self.extenderProvideMonitor.NotifyAll()
}

// watchExtenderProvideStatus emits at most one status per epoch, carrying the
// complete state (F3).
//
// It waits for a change, sleeps the epoch so a burst collapses, and only then
// re-arms and reads. Arming before the epoch would leave the next round
// already triggered by the same burst the snapshot already carries. See
// NetworkSpace.watchExtenderStatus for the same shape.
func (self *DeviceLocal) watchExtenderProvideStatus() {
	deviceUpdate := self.extenderProvideMonitor.NotifyChannel()
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
