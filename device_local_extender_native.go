//go:build !ios && !android && !js

package sdk

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	ma "github.com/multiformats/go-multiaddr"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extender"
	"github.com/urnetwork/connect/gossip"
)

// device_local_extender_native.go — the provider extender role (EXTENDER.md G1
// to G3).
//
// A desktop provider is already a host with a public address and spare
// capacity, which is exactly what an extender is. While it provides, and while
// the setting of F3 is on, it runs the open extender server of `connect/
// extender` on the three carriers, joins the mesh as a listening node, and
// proves itself to the operator on the activation cadence of G3.
//
// Only the operator decides what this extender is: the activation posts the
// carriers that actually bound, the operator probes back on the caller address
// of that post, and the signed record that comes back names the address this
// host is published under. Nothing here announces an address of its own.
//
// The parts are the ones connectctl's standalone extender wires (G4): the
// in-process gossip listener and the feed server behind the reserved services
// (A8), the space's node rebuilt in the extender role (D2), the extender
// server, the peer pinger with the reporter its pings go to (GEOMAP §2.1,
// §2.5), and the activation loop. The listener is stable for the life of the
// role, so a changed set of activated addresses rebuilds only the node.
//
// The role is safe for concurrent use. Close joins its loop and releases every
// part in the reverse order it was built.

// This build carries the role (G1).
const extenderProvideSupported = true

// Budget of the wait for the carriers to bind before the role gives up on
// activating. A bind that has not settled in this long is a bind that is not
// going to.
const extenderProvideListenTimeout = 60 * time.Second

type deviceLocalExtender struct {
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	done      chan struct{}
	log       connect.Logger

	settings  *deviceLocalExtenderSettings
	publicKey ed25519.PublicKey

	clientStrategy *connect.ClientStrategy
	listener       *gossip.InProcessListener
	feedServer     *gossip.FeedServer
	server         *extender.ExtenderServer
	// what this extender measures of its peers, and the reporter those
	// measurements are posted to the operator through (GEOMAP §2.1, §2.5)
	pingReporter *connect.ExtenderPingReporter
	peerPinger   *connect.ExtenderPeerPinger

	// the comparable half of the status; a consumer is woken only on an actual
	// change, and the connection count is read live beside it
	statusMonitor *connect.MonitorValue[extenderProvideState]
	// closed and replaced when something the status reads changed outside the
	// monitors the loop watches: a bind failure, an activation
	wakeMonitor *connect.Monitor

	stateLock sync.Mutex
	// true while the space runs a node for this role, so the gossip service is
	// served rather than refused (A8)
	nodeRuns bool
	// nil until the carriers have bound
	activator *connect.ExtenderActivator
	// the mesh addresses the node currently advertises, in text form
	nodeListenAddrs []string
	// when this extender's key was first seen revoked in the directory (G3)
	revokedTime time.Time

	// the space directory's cap on active records before this role replaced
	// it, restored when the role closes, and whether it was replaced (D26)
	previousMaxActiveRecordCount int
	maxActiveRecordCountReplaced bool
}

// The role is running when this returns: the server is binding its carriers,
// the node is up, and the loop that activates and publishes the status has
// started. When this space cannot run one, the error says why, and the
// provider reports it as the start error of N3.
func newDeviceLocalExtender(
	ctx context.Context,
	settings *deviceLocalExtenderSettings,
) (*deviceLocalExtender, error) {
	if settings == nil || settings.NetworkSpace == nil {
		return nil, errors.New("the device has no network space")
	}
	if settings.NetworkSpace.extenderDirectory == nil {
		// a space whose api url derives no extender network host (an ip
		// literal or a single label) keeps no directory to publish this
		// extender's own record into (F1)
		return nil, errors.New("the network space has no extender directory")
	}
	log := settings.Log
	if log == nil {
		log = connect.DefaultLogger()
	}
	// the identity key signs this extender's peer pings; the published key is
	// derived from it (B1, GEOMAP §2.2)
	privateKey, err := connect.ExtenderPrivateKeyFromSeed(settings.IdentityKeySeed)
	if err != nil {
		log.Infof("[extender]provide identity err = %s\n", err)
		return nil, fmt.Errorf("the extender identity is not usable: %s", err)
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)

	cancelCtx, cancel := context.WithCancel(ctx)
	self := &deviceLocalExtender{
		ctx:           cancelCtx,
		cancel:        cancel,
		done:          make(chan struct{}),
		log:           log,
		settings:      settings,
		publicKey:     publicKey,
		statusMonitor: connect.NewMonitorValue(extenderProvideState{Enabled: true}),
		wakeMonitor:   connect.NewMonitor(),
	}

	// direct only: an activation that crossed an extender would tell the
	// operator that extender's address, not this host's (C2)
	strategySettings := settings.ClientStrategySettings
	if strategySettings == nil {
		strategySettings = connect.DefaultClientStrategySettings()
	}
	self.clientStrategy = connect.NewDirectClientStrategy(cancelCtx, strategySettings, 0)

	self.listener = gossip.NewInProcessListener(cancelCtx, gossip.DefaultInProcessListenerSettings())
	self.feedServer = gossip.NewFeedServer(
		cancelCtx,
		settings.NetworkSpace.extenderDirectory,
		publicKey,
		gossip.DefaultFeedServerSettings(),
	)
	// the node is built before the server, because an extender with no node
	// must refuse the gossip service rather than queue it (A8)
	nodeRuns := settings.NetworkSpace.setExtenderNodeRole(self.listener, nil) != nil
	self.setNodeRuns(nodeRuns)

	self.server = extender.NewExtenderServer(
		cancelCtx,
		// an operator activated extender is open: it accepts every header and
		// forwards only to the whitelist (A4, A5)
		nil,
		settings.AllowedHosts,
		self.ports(),
		&net.Dialer{},
		self.serverSettings(nodeRuns),
	)
	go connect.HandleError(func() {
		if err := self.server.ListenAndServe(); err != nil {
			// every carrier failed to bind; the status carries which and why
			self.log.Infof("[extender]provide listen err = %s\n", err)
		}
		self.wakeMonitor.NotifyAll()
	}, cancel)

	// what this extender measures of its peers goes to the operator in
	// batches, under the same client credential the activation uses. The
	// pinger reports; what a provider measures of this extender that provider
	// reports itself (GEOMAP §2.5)
	reporterSettings := connect.DefaultExtenderPingReporterSettings()
	reporterSettings.Log = log
	reporterSettings.ApiUrl = extenderReporterApiUrl(settings)
	reporterSettings.ByJwt = settings.ByJwt
	reporterSettings.ClientStrategy = self.clientStrategy
	if settings.ConfigurePingReporter != nil {
		settings.ConfigurePingReporter(reporterSettings)
	}
	self.pingReporter = connect.NewExtenderPingReporter(cancelCtx, reporterSettings)

	// a peer judges this extender's pings by the same records this server
	// judges the peer's by, so they are signed as this extender, with the
	// identity key and under the peer probe domain only, and they start once
	// this extender's own record is active (GEOMAP §2.1, §2.4)
	pingerSettings := connect.DefaultExtenderPeerPingerSettings()
	pingerSettings.Log = log
	pingerSettings.OwnPublicKey = publicKey
	pingerSettings.Attestor = connect.NewExtenderProbeExtenderAttestor(
		publicKey,
		connect.NewExtenderPeerProbeSigner(privateKey),
	)
	pingerSettings.Reporter = self.pingReporter
	if settings.Now != nil {
		pingerSettings.Now = settings.Now
	}
	pingerSettings.IpVersionSupported = settings.IpVersionSupported
	// the bounded peer sample (GEOMAP §2.1, D26): zero keeps the connect
	// default and a negative value pings every peer
	switch {
	case settings.PeerSampleSize < 0:
		pingerSettings.PeerSampleSize = 0
	case 0 < settings.PeerSampleSize:
		pingerSettings.PeerSampleSize = settings.PeerSampleSize
	}
	if settings.ConfigurePeerPinger != nil {
		settings.ConfigurePeerPinger(pingerSettings)
	}
	self.peerPinger = connect.NewExtenderPeerPinger(
		cancelCtx,
		self.clientStrategy,
		self.directory(),
		pingerSettings,
	)

	// the space directory's cap on active records while the role runs (D26):
	// zero leaves it as the space has it, a negative value keeps every record,
	// and the space's own comes back when the role closes
	if settings.MaxActiveRecordCount != 0 {
		directory := self.directory()
		self.previousMaxActiveRecordCount = directory.MaxActiveRecordCount()
		self.maxActiveRecordCountReplaced = true
		directory.SetMaxActiveRecordCount(max(0, settings.MaxActiveRecordCount))
	}

	go connect.HandleError(func() {
		defer close(self.done)
		self.run()
	}, cancel)
	return self, nil
}

// The carrier ports (A1). tcp and udp share port 443 in production, which is
// one entry with both connect modes.
func (self *deviceLocalExtender) ports() map[int][]connect.ExtenderConnectMode {
	ports := map[int][]connect.ExtenderConnectMode{}
	for _, carrier := range []struct {
		port        int
		connectMode connect.ExtenderConnectMode
	}{
		{port: self.settings.TcpPort, connectMode: connect.ExtenderConnectModeTcpTls},
		{port: self.settings.UdpPort, connectMode: connect.ExtenderConnectModeQuic},
		{port: self.settings.DnsPort, connectMode: connect.ExtenderConnectModeDns},
	} {
		ports[carrier.port] = append(ports[carrier.port], carrier.connectMode)
	}
	return ports
}

// Whether the space still runs a node for this role. A rebuild that fails
// leaves the gossip service without a consumer, and a stream handed to a
// listener nothing accepts from would hold its connection until the role
// closes.
func (self *deviceLocalExtender) gossipNodeRuns() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.nodeRuns
}

func (self *deviceLocalExtender) setNodeRuns(nodeRuns bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.nodeRuns = nodeRuns
}

func (self *deviceLocalExtender) serverSettings(nodeRuns bool) *extender.ExtenderSettings {
	settings := extender.DefaultExtenderSettings()
	settings.IdentityKeySeed = self.settings.IdentityKeySeed
	settings.SpoofDomains = self.settings.SpoofDomains
	// 53 beside the configured unprivileged port where the platform allows it
	// (L2); a failure of that bind disables the one port, not the carrier
	settings.DnsPrivilegedPort = self.settings.DnsPrivilegedPort
	settings.DialContext = self.settings.DialContext
	settings.Listen = self.settings.Listen
	settings.ListenPacket = self.settings.ListenPacket
	// the admission limits of this instance (A12): zero keeps the connect
	// default and a negative value disables the limit
	for _, limit := range []struct {
		value int
		field *int
	}{
		{value: self.settings.AdmissionSubnetsPerMinute, field: &settings.AdmissionSubnetsPerMinute},
		{value: self.settings.AdmissionActionsPerSubnetPerMinute, field: &settings.AdmissionActionsPerSubnetPerMinute},
	} {
		switch {
		case limit.value < 0:
			*limit.field = 0
		case 0 < limit.value:
			*limit.field = limit.value
		}
	}
	settings.AdmissionUnlimitedSources = slices.Clone(self.settings.AdmissionUnlimitedSources)
	if self.settings.DnsTld != "" {
		settings.DnsTlds = []string{self.settings.DnsTld}
	}
	if nodeRuns {
		settings.GossipConnHandler = func(conn net.Conn) {
			if !self.gossipNodeRuns() {
				// the node went away; the extender closes the stream when this
				// returns, which is what a client retries elsewhere on
				return
			}
			self.listener.Handle(conn)
		}
	}
	settings.FeedConnHandler = self.feedServer.Serve
	// a peer that pings this extender is judged against the records the
	// operator vouched for, the same directory the feed serves (GEOMAP §2.4)
	settings.ProbePeerVerifier = self.directory().IsActiveKey
	// a bind failure is not a user-visible error: it disables that carrier,
	// is logged once by the server, and appears in the status (G2, F3)
	settings.ListenErrorHandler = func(carrier string, err error) {
		self.wakeMonitor.NotifyAll()
	}
	return settings
}

// The loop that activates and publishes the status. Everything it waits on is
// a monitor, and every pass reads the whole state, so a change that lands
// while a pass runs is carried into the next wait rather than lost.
func (self *deviceLocalExtender) run() {
	// the activation offers the carriers that bound, so it waits for the binds
	// to settle rather than announcing a list still being assembled (G2, G3)
	select {
	case <-self.server.Listening():
	case <-self.ctx.Done():
		return
	case <-time.After(extenderProvideListenTimeout):
		self.log.Infof("[extender]provide carriers did not bind\n")
	}
	self.publish()
	if len(self.server.Carriers()) == 0 {
		// nothing bound: there is nothing to activate and nothing to
		// advertise. The failure stands in the status until the role is
		// restarted by a provide or setting change.
		<-self.ctx.Done()
		return
	}
	self.startActivator()

	for {
		// subscribe before the reads below, so a change in between is carried
		// by the channel rather than lost
		wake := self.wakeMonitor.NotifyChannel()
		_, activatorUpdate := self.activatorChangeMonitor().Get()
		var directoryUpdate chan struct{}
		if directory := self.directory(); directory != nil {
			_, directoryUpdate = directory.ChangeMonitor().Get()
		}
		// the ping counts the status carries (GEOMAP §2.1)
		_, pingerUpdate := self.peerPinger.StatusMonitor().Get()

		self.updateNode()
		self.publish()

		select {
		case <-self.ctx.Done():
			return
		case <-wake:
		case <-activatorUpdate:
		case <-directoryUpdate:
		case <-pingerUpdate:
		}
	}
}

// Builds the activation loop over the carriers that bound (G3).
func (self *deviceLocalExtender) startActivator() {
	settings := connect.DefaultExtenderActivatorSettings()
	settings.Log = self.log
	settings.ApiUrlV4 = self.settings.ApiUrlV4
	settings.ApiUrlV6 = self.settings.ApiUrlV6
	settings.ApiUrl = self.settings.ApiUrl
	settings.HelloUrl = self.settings.HelloUrl
	settings.ByJwt = self.settings.ByJwt
	settings.ClientStrategy = self.clientStrategy
	settings.PublicKey = self.publicKey
	settings.TcpPort = self.settings.TcpPort
	settings.UdpPort = self.settings.UdpPort
	settings.DnsPort = self.settings.DnsPort
	if self.settings.DnsTld != "" {
		settings.DnsTld = self.settings.DnsTld
	}
	settings.Carriers = self.server.Carriers
	// the operator probes each port that actually bound and the record lists
	// them, so a privileged bind that failed is never advertised (L2)
	settings.DnsPorts = self.server.DnsPorts
	settings.Directory = self.directory()
	// the mesh addresses follow the activated families, which the loop reads
	// from the activator's own status; this only wakes it (D2, G2)
	settings.OnActivated = func(ipVersion int, result *connect.ExtenderActivateResult) {
		self.wakeMonitor.NotifyAll()
	}
	if 0 < self.settings.ActivateTimeout {
		settings.ActivateTimeout = self.settings.ActivateTimeout
	}
	if 0 < self.settings.AddressCheckTimeout {
		settings.AddressCheckTimeout = self.settings.AddressCheckTimeout
	}
	if 0 < self.settings.MinBackoff {
		settings.MinBackoff = self.settings.MinBackoff
	}
	if 0 < self.settings.MaxBackoff {
		settings.MaxBackoff = self.settings.MaxBackoff
	}
	if 0 < self.settings.RequestTimeout {
		settings.RequestTimeout = self.settings.RequestTimeout
	}
	if self.settings.Now != nil {
		settings.Now = self.settings.Now
	}
	settings.IpVersionSupported = self.settings.IpVersionSupported

	activator := connect.NewExtenderActivator(self.ctx, settings)
	self.stateLock.Lock()
	self.activator = activator
	self.stateLock.Unlock()
	self.wakeMonitor.NotifyAll()
}

func (self *deviceLocalExtender) directory() *connect.ExtenderDirectory {
	return self.settings.NetworkSpace.extenderDirectory
}

func (self *deviceLocalExtender) currentActivator() *connect.ExtenderActivator {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.activator
}

// The activator's change counter, or a monitor that never fires before the
// activator exists, so the loop's subscribe is unconditional.
func (self *deviceLocalExtender) activatorChangeMonitor() *connect.MonitorValue[uint64] {
	if activator := self.currentActivator(); activator != nil {
		return activator.ChangeMonitor()
	}
	return connect.NewMonitorValue[uint64](0)
}

func (self *deviceLocalExtender) activatorStatus() *connect.ExtenderActivatorStatus {
	if activator := self.currentActivator(); activator != nil {
		return activator.Status()
	}
	return &connect.ExtenderActivatorStatus{}
}

// Rebuilds the node whenever the set of activated addresses changes (G2, D2).
// A libp2p host can add a listen address but not drop one, so a deactivated or
// moved family is a rebuild: otherwise this node would keep advertising a mesh
// address the operator no longer publishes for it.
func (self *deviceLocalExtender) updateNode() {
	if !self.gossipNodeRuns() {
		return
	}
	listenAddrs, err := self.meshListenAddrs()
	if err != nil {
		self.log.Infof("[extender]provide mesh address err = %s\n", err)
		return
	}
	listenAddrTexts := []string{}
	for _, listenAddr := range listenAddrs {
		listenAddrTexts = append(listenAddrTexts, listenAddr.String())
	}

	changed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if slices.Equal(self.nodeListenAddrs, listenAddrTexts) {
			return false
		}
		self.nodeListenAddrs = listenAddrTexts
		return true
	}()
	if !changed {
		return
	}
	self.setNodeRuns(
		self.settings.NetworkSpace.setExtenderNodeRole(self.listener, listenAddrs) != nil)
}

// One mesh address per activated family (D2). Only the tcp carrier carries the
// mesh, so a host whose tcp bind failed advertises nothing.
func (self *deviceLocalExtender) meshListenAddrs() ([]ma.Multiaddr, error) {
	if !slices.Contains(self.server.Carriers(), connect.ExtenderCarrierTcp) {
		return nil, nil
	}
	ips := []netip.Addr{}
	for _, family := range self.activatorStatus().Families {
		if family.Activated && family.Ip.IsValid() {
			ips = append(ips, family.Ip)
		}
	}
	if len(ips) == 0 {
		return nil, nil
	}
	return gossip.ExtenderListenAddrs(ips, self.settings.TcpPort)
}

// Publishes the comparable half of the status, which notifies a watcher only
// when something actually changed.
func (self *deviceLocalExtender) publish() {
	self.statusMonitor.Set(self.state())
}

func (self *deviceLocalExtender) state() extenderProvideState {
	state := extenderProvideState{
		Enabled:     true,
		Listening:   0 < len(self.server.Carriers()),
		ListenError: extenderListenErrorText(self.server.ListenErrors()),
		DnsPorts:    extenderDnsPortsText(self.server.DnsPorts()),
	}

	var lastActivationTime time.Time
	var lastErrorTime time.Time
	for _, family := range self.activatorStatus().Families {
		switch family.IpVersion {
		case 4:
			state.ActivatedV4 = family.Activated
			if family.Activated && family.Ip.IsValid() {
				state.Ipv4 = family.Ip.String()
			}
		case 6:
			state.ActivatedV6 = family.Activated
			if family.Activated && family.Ip.IsValid() {
				state.Ipv6 = family.Ip.String()
			}
		}
		if family.LastActivationTime.After(lastActivationTime) {
			lastActivationTime = family.LastActivationTime
		}
		// the newest error any family is still standing on, so a v6 refusal
		// stays visible while v4 is activated. A family that succeeds clears
		// its own error, so nothing stale survives here.
		if family.LastError != "" && !family.LastActivationTime.Before(lastErrorTime) {
			lastErrorTime = family.LastActivationTime
			state.LastActivationError = family.LastError
			// whether that error is the operator's refusal, from the same
			// family, so the case the rule picks matches the text (N2, N3)
			state.LastActivationRefused = family.LastRefused
		}
	}
	state.LastActivationTime = extenderStatusTimeMs(lastActivationTime)
	state.RevokedTime = extenderStatusTimeMs(self.updateRevoked())

	// what this extender measured of its peers and what they answered
	// (GEOMAP §2.1, §2.3)
	pingerStatus := self.peerPinger.Status()
	state.PeerPingCount = pingerStatus.PingCount
	state.PeerPingCosignedCount = pingerStatus.CosignedCount
	state.PeerPingRejectedCount = pingerStatus.RejectedCount
	state.PeerPingUnknownCount = pingerStatus.UnknownCount
	state.LastPeerPingTime = extenderStatusTimeMs(pingerStatus.LastPingTime)

	// what the admission limits turned away (A12), read at each publish; a
	// refusal alone wakes nothing, since a flood would be a publish per
	// refused connection
	admissionStats := self.server.AdmissionStats()
	state.LimitedBySubnetsCount = int(admissionStats.LimitedBySubnetsCount)
	state.LimitedBySourceCount = int(admissionStats.LimitedBySourceCount)
	return state
}

// When this extender's key was first seen revoked in the directory, zero while
// it is not revoked (G3, B5). The directory is the source: the operator's
// revocation reaches this host through the mesh or the feed, not through an
// answer to something this host asked.
func (self *deviceLocalExtender) updateRevoked() time.Time {
	revoked := false
	if directory := self.directory(); directory != nil && 0 < len(self.publicKey) {
		for _, entry := range directory.Snapshot().Entries {
			if entry.State == connect.ExtenderStateRevoked &&
				slices.Equal(entry.PublicKey, self.publicKey) {
				revoked = true
				break
			}
		}
	}

	now := time.Now
	if self.settings.Now != nil {
		now = self.settings.Now
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch {
	case !revoked:
		self.revokedTime = time.Time{}
	case self.revokedTime.IsZero():
		self.revokedTime = now()
	}
	return self.revokedTime
}

// The status as the device renders it (F3). The connection count is read live,
// beside the state the loop publishes.
func (self *deviceLocalExtender) status() *ExtenderProvideStatus {
	if self == nil {
		return disabledExtenderProvideStatus()
	}
	return self.statusMonitor.Value().status(self.server.ConnectionCount())
}

// The relayed traffic of this role's server (O2): read off the server's
// atomics, so no lock is taken and a server that has closed still answers.
func (self *deviceLocalExtender) stats() *ExtenderStats {
	if self == nil {
		return nil
	}
	stats := self.server.Stats()
	return &ExtenderStats{
		IngressByteCount: stats.IngressByteCount,
		IngressReadCount: stats.IngressReadCount,
		EgressByteCount:  stats.EgressByteCount,
		EgressReadCount:  stats.EgressReadCount,
	}
}

// A channel armed at the instant of the read, so a consumer is woken when the
// status changes. A device with no role waits on nil, which never fires.
func (self *deviceLocalExtender) statusUpdate() chan struct{} {
	if self == nil {
		return nil
	}
	_, update := self.statusMonitor.Get()
	return update
}

// Releases every part in the reverse order it was built, and restores the
// member node the role replaced (G2).
func (self *deviceLocalExtender) Close() {
	if self == nil {
		return
	}
	self.closeOnce.Do(func() {
		self.cancel()
		<-self.done
		if activator := self.currentActivator(); activator != nil {
			activator.Close()
		}
		// a ping in flight reports into the reporter, and both post and dial
		// through the strategy, so the pinger is joined first and the strategy
		// last
		self.peerPinger.Close()
		self.pingReporter.Close()
		self.server.CloseAndWait()
		// the node holds the listener, so it goes before the listener does
		self.settings.NetworkSpace.restoreExtenderNodeRole()
		self.feedServer.Close()
		self.listener.Close()
		self.clientStrategy.Close()
		if self.maxActiveRecordCountReplaced {
			self.directory().SetMaxActiveRecordCount(self.previousMaxActiveRecordCount)
		}
	})
}

// The api url the ping report is posted to: the first this role has. A report
// may arrive on either family, so a family url serves when there is no other.
func extenderReporterApiUrl(settings *deviceLocalExtenderSettings) string {
	for _, apiUrl := range []string{settings.ApiUrl, settings.HelloUrl, settings.ApiUrlV4, settings.ApiUrlV6} {
		if apiUrl != "" {
			return apiUrl
		}
	}
	return ""
}
