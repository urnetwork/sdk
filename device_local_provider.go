package sdk

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	// "github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// deviceLocalP2pUdpSocketBufferByteCount is the per-socket kernel send and
// receive buffer requested for ICE UDP sockets on a device.
const deviceLocalP2pUdpSocketBufferByteCount = 512 * 1024

// bound on waiting for a migration replacement transport to connect before
// keeping the old transport (the draining server then evicts, and the
// reconnect falls back to the drain excuse path)
const platformTransportMigrateConnectTimeout = 60 * time.Second
const platformTransportMigrateMaxScheduleDelay = 5 * time.Minute

type migratablePlatformTransport interface {
	ConnectedNotify() <-chan struct{}
	IsConnected() bool
	SetAuth(auth *connect.ClientAuth)
	Close()
}

// The provider half of the device: its own client, its platform transport
// group and the extender role that runs beside it.
//
// A provider whose provide mode includes public never dials the platform
// through an extender or a proxy on any transport, the standby included, so
// the platform observes the provider's own address and location
// (EXTENDER.md J4). The pinned transports of the group are direct by
// construction (IPV6.md A4); the standby takes the provider's own direct-only
// strategy while the mode includes public and the device's shared strategy
// otherwise, since a network or friends-and-family provider carries no
// location metadata and may keep using extenders. A mode change that flips
// that rebuilds the transports through the migration path.
type deviceLocalProvider struct {
	ctx    context.Context
	cancel context.CancelFunc
	// this is the client for provide
	client       *connect.Client
	clientOob    *connect.ApiOutOfBandControl
	localUserNat *connect.LocalUserNat

	appVersion string
	instanceId connect.Id

	// the space this provider belongs to, which the extender role takes its
	// identity, directory, node and operator urls from (G2)
	networkSpace *NetworkSpace
	// the device's egress-aware dial, which is also the extender relay's
	// forward dial so the relay never enters the device's own tunnel (G2)
	dialContextSettings *connect.DialContextSettings
	// extenderSettingsConfigure, when set, adjusts the extender role's
	// settings before it is built. Tests bind ephemeral carrier ports and
	// point the activation at an in-process operator through it.
	extenderSettingsConfigure func(settings *deviceLocalExtenderSettings)

	clientStrategy *connect.ClientStrategy
	// clientStrategySettings seeds the direct-only strategies of the
	// family-pinned transports; nil falls back to the connect defaults
	clientStrategySettings *connect.ClientStrategySettings
	platformUrl            string
	// platformUrlV4 and platformUrlV6 are the family-pinned urls (IPV6.md
	// A2, A9). Empty disables that pinned transport, and with both empty the
	// group is the legacy single transport.
	platformUrlV4             string
	platformUrlV6             string
	platformTransportSettings *connect.PlatformTransportSettings
	targetMode                connect.TransportMode
	modePreferences           map[connect.TransportMode]int
	transportPolicyVersion    uint64

	// a migrate frame spawns at most one in-flight migration
	migrating atomic.Bool
	// bound on waiting for the replacement to connect
	// (default `platformTransportMigrateConnectTimeout`)
	migrateConnectTimeout time.Duration
	// bound on a server-provided absolute migrate time. This is a little over
	// twice the server's default two-minute jitter window, so clock skew or a
	// malformed far-future timestamp cannot pin migrating indefinitely.
	migrateMaxScheduleDelay time.Duration
	// injectable for deterministic migration tests; nil uses the production
	// PlatformTransport constructor.
	newPlatformTransport func(
		auth *connect.ClientAuth,
		targetMode connect.TransportMode,
		settings *connect.PlatformTransportSettings,
	) migratablePlatformTransport

	// extenderLock serializes the extender role's start and stop, which both
	// build and join external objects. It is always taken before stateLock,
	// which guards only the pointer a status read sees.
	extenderLock sync.Mutex

	stateLock sync.Mutex
	// the extender role while it runs (G2), nil while provide or the setting
	// is off and on every build that does not carry it (G1)
	extender *deviceLocalExtender
	// why the role could not start while it was asked to run, empty while it
	// runs or was not asked to (N3)
	extenderStartError string
	// the device's effective provide mode, which decides whether the standby
	// dials direct (J4). The device hands it over on every change.
	provideMode ProvideMode
	// the direct-only standby strategy of a public provider (J4), built on
	// the first public transport generation, reused by every later one, and
	// closed with the provider. It outlives a flip back to a non-public mode
	// so a flip forward keeps its connect pacing and costs no rebuild of the
	// strategy itself.
	directStandbyStrategy *connect.ClientStrategy
	closed                bool
	auth                  *connect.ClientAuth
	authVersion           uint64
	platformTransport     migratablePlatformTransport
	h1ConnectionStats     connect.H1ConnectionStats
	migrationWorkers      sync.WaitGroup
	closeOnce             sync.Once
	joinOnce              sync.Once
	closeDoneOnce         sync.Once
	closeDone             chan struct{}

	// the provider client's own transfer budget pair, when sized from the
	// provider share of the device memory target (see
	// newDeviceLocalProviderWithOverrides). nil when the provider shares the
	// device client budgets (no target).
	resendQueueBudget  *connect.TransferMemoryBudget
	receiveQueueBudget *connect.TransferMemoryBudget
}

// transferBudgets returns the provider client's own budget pair, or nils
// when the provider shares the device client budgets
func (self *deviceLocalProvider) transferBudgets() (resendQueueBudget *connect.TransferMemoryBudget, receiveQueueBudget *connect.TransferMemoryBudget) {
	return self.resendQueueBudget, self.receiveQueueBudget
}

func newDeviceLocalProviderWithOverrides(
	ctx context.Context,
	networkSpace *NetworkSpace,
	byJwt string,
	appVersion string,
	instanceId connect.Id,
	settings *connect.ClientSettings,
	clientId connect.Id,
	providerMemoryTargetByteCount ByteCount,
	deviceMemoryTargetByteCount ByteCount,
	platformTransportBudget *connect.PlatformTransportBudget,
	targetMode connect.TransportMode,
	modePreferences map[connect.TransportMode]int,
	dialContextSettings *connect.DialContextSettings,
	dnsPumpHost string,
	transferMemory *deviceLocalTransferMemory,
) (*deviceLocalProvider, error) {
	providerCtx, providerCancel := context.WithCancel(ctx)
	apiUrl := networkSpace.apiUrl
	clientStrategy := networkSpace.clientStrategy

	clientOob := connect.NewApiOutOfBandControl(providerCtx, clientStrategy, byJwt, apiUrl)

	clientSettings := newDeviceClientSettings(settings, apiUrl, clientStrategy)
	// A controlled mobile provider pinned to explicit H1 must select the same
	// negotiated flow lanes as its client peer; otherwise only request/TCP-ACK
	// traffic is isolated and every download still shares provider lane zero.
	// Auto and H3 remain unchanged until the provider-side physical A/B proves a
	// safe default across carrier fallback.
	applyMobileH1PerformanceClientSettings(
		clientSettings,
		deviceMemoryTargetByteCount,
		targetMode == connect.TransportModeH1,
	)
	// the provider always enables the e2e encryption sessions: the responder
	// serves plain and e2e peers seamlessly (a session only forms when an
	// initiator starts a handshake), and every enabled provider grows the
	// e2e-capable pool for pqe initiators
	if clientSettings.EncryptionSettings == nil {
		clientSettings.EncryptionSettings = connect.DefaultEncryptionSettings()
	}
	clientSettings.EncryptionSettings.Mode = connect.EncryptionModeOpportunistic
	// This top-level client exists to provide/relay traffic. Apply provide-mode
	// reductions to every P2P stream direction, including stale companion
	// return streams restored by StreamReset after a process restart. Window
	// clients created for an outbound destination leave this false.
	clientSettings.ProviderStreamPolicy = true
	var providerTransferParent *connect.TransferMemoryBudget
	if transferMemory != nil {
		providerTransferParent = transferMemory.provider
	}

	resendQueueBudget, receiveQueueBudget := configureDeviceLocalProviderMemory(
		clientSettings,
		providerMemoryTargetByteCount,
		providerTransferParent,
	)
	// Admit the fallback NAT while the new device root is still empty, before
	// starting the provider client. It is a lifetime owner even with providing
	// disabled. Refusal is observable; never install a dead/nil NAT as a route.
	localUserNatSettings := providerLocalUserNatSettings(
		providerMemoryTargetByteCount,
		clientSettings.Log,
	)
	if transferMemory != nil {
		localUserNatSettings.MemoryBudget = transferMemory.nat
	}
	localUserNat, err := connect.TryNewLocalUserNat(providerCtx, clientId.String(), localUserNatSettings)
	if err != nil {
		providerCancel()
		_ = clientOob.CloseAndWait(context.Background())
		return nil, fmt.Errorf("admit fallback NAT: %w", err)
	}

	client := connect.NewClient(
		providerCtx,
		clientId,
		clientOob,
		clientSettings,
	)

	auth := &connect.ClientAuth{
		ByJwt:      byJwt,
		InstanceId: instanceId,
		AppVersion: appVersion,
	}
	platformTransportSettings := newDeviceLocalPlatformTransportSettings(
		deviceMemoryTargetByteCount,
		platformTransportBudget,
		dialContextSettings,
		networkSpace.GetAltUrl(),
		dnsPumpHost,
	)
	platformTransportSettings.Log = clientSettings.Log
	platformTransportSettings.ModePreferences = maps.Clone(modePreferences)
	// The provider exists before outbound client windows. Its optional Auto-H3
	// lease must therefore be reclaimable by foreground client Auto/H3 demand;
	// otherwise creation order permanently leaves every outbound window on H1.
	// Explicit provider H3 is a required reservation and ignores this priority.
	platformTransportSettings.PlatformTransportBudgetPriority =
		connect.PlatformTransportBudgetPriorityBackground

	provider := &deviceLocalProvider{
		ctx:          providerCtx,
		cancel:       providerCancel,
		client:       client,
		clientOob:    clientOob,
		localUserNat: localUserNat,

		appVersion: appVersion,
		instanceId: instanceId,

		networkSpace:              networkSpace,
		dialContextSettings:       dialContextSettings,
		clientStrategy:            clientStrategy,
		clientStrategySettings:    networkSpace.clientStrategySettings,
		platformUrl:               networkSpace.platformUrl,
		platformUrlV4:             networkSpace.GetPlatformUrlV4(),
		platformUrlV6:             networkSpace.GetPlatformUrlV6(),
		platformTransportSettings: platformTransportSettings,
		targetMode:                targetMode,
		modePreferences:           maps.Clone(modePreferences),
		transportPolicyVersion:    1,
		migrateConnectTimeout:     platformTransportMigrateConnectTimeout,
		migrateMaxScheduleDelay:   platformTransportMigrateMaxScheduleDelay,
		auth:                      auth,
		resendQueueBudget:         resendQueueBudget,
		receiveQueueBudget:        receiveQueueBudget,
	}
	// the provider proves both address families through its family-pinned
	// transports (IPV6.md A1, A4); nothing else runs on the provider yet, so
	// the transport is installed without the lock
	platformTransportSettings.H1ConnectionStats = &provider.h1ConnectionStats
	provider.platformTransport = provider.newProviderPlatformTransport(
		auth,
		targetMode,
		platformTransportSettings,
	)
	// the platform asks the client to migrate its transport when the resident
	// is draining (make-before-break, CONNECTDRAIN2.md §3.3)
	client.AddReceiveCallback(provider.handleControlFrames)
	return provider, nil
}

// Whether a provide mode serves public peers (J4). The modes are ordered by
// openness, so the test is public and above, which also covers the stream
// modes. It errs toward public on purpose: dialing direct where it was not
// needed costs nothing, while a public provider tagged with an extender's
// address is the failure this rule exists to prevent.
func provideModeIncludesPublic(provideMode ProvideMode) bool {
	return ProvideModePublic <= provideMode
}

// The strategy the standby transport of a new generation dials with (J4).
// While the provide mode includes public it is the provider's own direct-only
// strategy -- no extender dialers, no proxy -- so the platform observes the
// provider's own address and location on every transport; every other mode
// keeps the device's shared strategy and its extender dialers.
//
// The direct strategy is built once and reused by every later generation, and
// is closed with the provider. It is built with no lock held, because the
// strategy constructor subscribes to network changes; a loser of the race
// closes its own build.
func (self *deviceLocalProvider) standbyClientStrategy(
	clientStrategySettings *connect.ClientStrategySettings,
) *connect.ClientStrategy {
	directStandbyStrategy, public := func() (*connect.ClientStrategy, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.directStandbyStrategy, provideModeIncludesPublic(self.provideMode)
	}()
	if !public {
		return self.clientStrategy
	}
	if directStandbyStrategy != nil {
		return directStandbyStrategy
	}
	directStandbyStrategy = connect.NewDirectClientStrategy(
		self.client.Ctx(),
		clientStrategySettings,
		0,
	)
	installed := func() *connect.ClientStrategy {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.directStandbyStrategy == nil {
			self.directStandbyStrategy = directStandbyStrategy
		}
		return self.directStandbyStrategy
	}()
	if installed != directStandbyStrategy {
		directStandbyStrategy.Close()
	}
	return installed
}

// Records the device's effective provide mode. When the public flag flips, the
// transports are rebuilt make-before-break so the new generation's standby
// carries the right strategy (J4); a change that leaves the flag alone only
// records the mode. The policy version is bumped for the
// same reason SetTransportPolicy bumps it: a migration already in flight then
// sees the change and repeats with the new mode instead of installing a
// generation built for the old one.
func (self *deviceLocalProvider) setProvideMode(provideMode ProvideMode) {
	flipped := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed || self.provideMode == provideMode {
			return false
		}
		flipped := provideModeIncludesPublic(self.provideMode) !=
			provideModeIncludesPublic(provideMode)
		self.provideMode = provideMode
		if flipped {
			self.transportPolicyVersion += 1
		}
		return flipped
	}()
	if flipped {
		self.requestPlatformTransportMigration(time.Now())
	}
}

// newProviderPlatformTransport builds the provider's transport group: the
// v4 and v6 pinned transports when the network space derives family urls,
// plus the family-agnostic standby. Both construction and migration build
// through here so a replacement carries the same pins as the original.
func (self *deviceLocalProvider) newProviderPlatformTransport(
	auth *connect.ClientAuth,
	targetMode connect.TransportMode,
	settings *connect.PlatformTransportSettings,
) migratablePlatformTransport {
	clientStrategySettings := self.clientStrategySettings
	if clientStrategySettings == nil {
		clientStrategySettings = connect.DefaultClientStrategySettings()
	}
	return connect.NewFamilyPlatformTransportGroup(
		self.client.Ctx(),
		clientStrategySettings,
		self.standbyClientStrategy(clientStrategySettings),
		self.client.RouteManager(),
		self.platformUrl,
		self.platformUrlV4,
		self.platformUrlV6,
		auth,
		targetMode,
		settings,
		nil,
	)
}

// canMakeBeforeBreak reports whether `next` may be brought up while
// `previous` keeps carrying traffic. A group pairs each transport with its
// counterpart; across a group and a single transport the standby, which is
// the family-agnostic carrier a policy replacement is bounded by, stands in
// for the group. Unknown (test) transports never force break-before-make.
func canMakeBeforeBreak(next migratablePlatformTransport, previous migratablePlatformTransport) bool {
	switch nextTransport := next.(type) {
	case *connect.FamilyPlatformTransportGroup:
		switch previousTransport := previous.(type) {
		case *connect.FamilyPlatformTransportGroup:
			return nextTransport.CanMakeBeforeBreakFrom(previousTransport)
		case *connect.PlatformTransport:
			return nextTransport.StandbyTransport().CanMakeBeforeBreakFrom(previousTransport)
		}
	case *connect.PlatformTransport:
		switch previousTransport := previous.(type) {
		case *connect.PlatformTransport:
			return nextTransport.CanMakeBeforeBreakFrom(previousTransport)
		case *connect.FamilyPlatformTransportGroup:
			return nextTransport.CanMakeBeforeBreakFrom(previousTransport.StandbyTransport())
		}
	}
	return true
}

// familyTransportStatus is the per-family readout of the current transport
// generation. A legacy single transport (a test seam, or a space without
// family urls handled by an older build) reports through its connected bit.
func (self *deviceLocalProvider) familyTransportStatus() *ProviderFamilyTransportStatus {
	self.stateLock.Lock()
	closed := self.closed
	platformTransport := self.platformTransport
	self.stateLock.Unlock()
	if closed || platformTransport == nil {
		return unknownProviderFamilyTransportStatus()
	}
	if group, ok := platformTransport.(*connect.FamilyPlatformTransportGroup); ok {
		return newProviderFamilyTransportStatus(group.Status())
	}
	return legacyProviderFamilyTransportStatus(platformTransport.IsConnected())
}

// configureDeviceLocalProviderMemory applies all provider-owned queue and P2P
// budgets in one testable step. Without a target the provider keeps the
// historical wiring and shares the budgets carried in on the copied settings.
func configureDeviceLocalProviderMemory(
	clientSettings *connect.ClientSettings,
	memoryTargetByteCount ByteCount,
	parents ...*connect.TransferMemoryBudget,
) (resendQueueBudget *connect.TransferMemoryBudget, receiveQueueBudget *connect.TransferMemoryBudget) {
	if memoryTargetByteCount <= 0 {
		return
	}

	// Half the provider share is the transfer pair, split 3:4 send:receive;
	// egress NAT flow caps own the other half.
	pairTarget := memoryTargetByteCount / 2
	resendQueueBudget = deviceLocalTransferBudgetWithParent(max(byteCountFraction(pairTarget, 3, 7), 256*1024), parents...)
	receiveQueueBudget = deviceLocalTransferBudgetWithParent(max(byteCountFraction(pairTarget, 4, 7), 384*1024), parents...)
	clientSettings.SendBufferSettings.ResendQueueBudget = resendQueueBudget
	clientSettings.ReceiveBufferSettings.ReceiveQueueBudget = receiveQueueBudget

	// Public P2P connections admit against a dedicated phone-sized pool, not
	// the active transfer receive queue (which is legitimately full precisely
	// when P2P is needed).
	clientSettings.WebRtcSettings.ReceiveBufferSize = deviceLocalP2pReceiveBufferByteCount
	clientSettings.WebRtcSettings.MemoryBudget = deviceLocalWebRtcBudget(memoryTargetByteCount, parents...)
	// ICE UDP socket buffers: 4 MiB is the server default; a phone keeps the
	// provider's ACK drops away at 512 KiB per socket without the footprint
	// of a gathered candidate set at server size (FLIGHTGATEFIX §13.7,
	// finding 5; MEMSTEADY gates it).
	clientSettings.WebRtcSettings.UdpSocketBufferByteCount = deviceLocalP2pUdpSocketBufferByteCount

	// A trusted ProvideMode_Network peer gets the symmetric selected-peer
	// window from its own bounded two-connection pool. It cannot enlarge or
	// starve the many-peer public pool.
	clientSettings.WebRtcSettings.NetworkPeerReceiveBufferSize =
		deviceLocalNetworkPeerP2pReceiveBufferByteCount
	clientSettings.WebRtcSettings.NetworkPeerMemoryBudget = deviceLocalTransferBudgetWithParent(
		deviceLocalNetworkPeerP2pConnectionCount*deviceLocalNetworkPeerP2pReceiveBufferByteCount,
		parents...,
	)
	return
}

// ReceiveFunction
func (self *deviceLocalProvider) handleControlFrames(source connect.TransferPath, frames []*protocol.Frame, peer connect.Peer) {
	if !source.IsControlSource() {
		return
	}
	for _, frame := range frames {
		if frame.MessageType != protocol.MessageType_TransferResidentMigrate {
			continue
		}
		message, err := connect.FromFrame(frame)
		if err != nil {
			continue
		}
		residentMigrate, ok := message.(*protocol.ResidentMigrate)
		if !ok {
			continue
		}
		migrateTime := time.UnixMilli(int64(residentMigrate.MigrateTime))
		self.requestPlatformTransportMigration(migrateTime)
	}
}

func (self *deviceLocalProvider) requestPlatformTransportMigration(migrateTime time.Time) {
	self.stateLock.Lock()
	if self.closed || !self.migrating.CompareAndSwap(false, true) {
		self.stateLock.Unlock()
		return
	}
	self.migrationWorkers.Add(1)
	self.stateLock.Unlock()
	go connect.HandleError(func() {
		defer self.migrationWorkers.Done()
		defer self.migrating.Store(false)
		for {
			attemptedPolicyVersion := self.migratePlatformTransportWithPolicy(migrateTime)
			self.stateLock.Lock()
			currentPolicyVersion := self.transportPolicyVersion
			self.stateLock.Unlock()
			if attemptedPolicyVersion == 0 || attemptedPolicyVersion == currentPolicyVersion {
				return
			}
			// The policy changed while the replacement was pending. Apply the
			// latest policy immediately; do not replay server migration jitter.
			migrateTime = time.Now()
		}
	})
}

// migratePlatformTransport performs make-before-break at `migrateTime`: build
// a replacement platform transport while the current one keeps carrying
// traffic, wait for the replacement to connect (bounded), then close the old
// transport so its routes drop and traffic continues over the replacement.
// On timeout the replacement is closed and the old transport stays: the
// draining server evicts it, and the reconnect falls back to the drain excuse
// path (CONNECTDRAIN2.md §3.3).
func (self *deviceLocalProvider) migratePlatformTransport(migrateTime time.Time) {
	self.migratePlatformTransportWithPolicy(migrateTime)
}

func (self *deviceLocalProvider) migratePlatformTransportWithPolicy(migrateTime time.Time) uint64 {
	maxScheduleDelay := self.migrateMaxScheduleDelay
	if maxScheduleDelay <= 0 {
		maxScheduleDelay = platformTransportMigrateMaxScheduleDelay
	}
	if latest := time.Now().Add(maxScheduleDelay); latest.Before(migrateTime) {
		migrateTime = latest
	}
	if wait := time.Until(migrateTime); 0 < wait {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-self.ctx.Done():
			return 0
		case <-timer.C:
		}
	}

	auth, authVersion, targetMode, modePreferences, policyVersion, platformTransportSettings := func() (
		*connect.ClientAuth,
		uint64,
		connect.TransportMode,
		map[connect.TransportMode]int,
		uint64,
		*connect.PlatformTransportSettings,
	) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		auth := *self.auth
		settings := *connect.DefaultPlatformTransportSettings()
		if self.platformTransportSettings != nil {
			settings = *self.platformTransportSettings
		}
		settings.ModePreferences = maps.Clone(self.modePreferences)
		targetMode := self.targetMode
		if targetMode == connect.TransportModeNone {
			targetMode = connect.TransportModeAuto
		}
		return &auth,
			self.authVersion,
			targetMode,
			maps.Clone(self.modePreferences),
			self.transportPolicyVersion,
			&settings
	}()
	platformTransportSettings.ModePreferences = modePreferences
	var next migratablePlatformTransport
	if self.newPlatformTransport != nil {
		next = self.newPlatformTransport(auth, targetMode, platformTransportSettings)
	} else {
		next = self.newProviderPlatformTransport(auth, targetMode, platformTransportSettings)
	}
	brokeBeforeMake := false
	func() {
		self.stateLock.Lock()
		previous := self.platformTransport
		self.stateLock.Unlock()
		if previous != nil && !canMakeBeforeBreak(next, previous) {
			// A second full H3 working set would escape the shared memory cap.
			// H1 transitions use Connect's bounded handoff and keep the old route;
			// only a budget-blocked H3-to-H3-family transition breaks first.
			closeMigratablePlatformTransportAndWait(previous)
			brokeBeforeMake = true
		}
	}()
	installNext := func() (migratablePlatformTransport, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return nil, false
		}
		// Token refresh can race replacement construction/connection. Reapply
		// the current immutable auth while holding the same lock used by
		// SetByJwt and the swap; a later refresh therefore updates next, while
		// an earlier one cannot be overwritten by the captured stale auth.
		if authVersion != self.authVersion {
			next.SetAuth(self.auth)
		}
		previous := self.platformTransport
		self.platformTransport = next
		return previous, true
	}

	connectEndTime := time.Now().Add(self.migrateConnectTimeout)
	for {
		notify := next.ConnectedNotify()
		if next.IsConnected() {
			break
		}
		if connectEndTime.Before(time.Now()) {
			if brokeBeforeMake {
				// The old full-H3 carrier is already closed to honor the memory
				// cap. Keep the replacement installed so its owned reconnect loop
				// continues instead of leaving a closed source as current.
				if _, installed := installNext(); !installed {
					closeMigratablePlatformTransportAndWait(next)
				}
				return policyVersion
			}
			// the replacement did not come up; keep the old transport
			closeMigratablePlatformTransportAndWait(next)
			return policyVersion
		}
		select {
		case <-self.ctx.Done():
			closeMigratablePlatformTransportAndWait(next)
			return policyVersion
		case <-notify:
		case <-time.After(1 * time.Second):
		}
	}

	previous, installed := installNext()
	if !installed {
		closeMigratablePlatformTransportAndWait(next)
		return policyVersion
	}
	if previous != nil && !brokeBeforeMake {
		closeMigratablePlatformTransportAndWait(previous)
	}
	return policyVersion
}

// SetTransportPolicy applies a provider carrier policy make-before-break. A
// duplicate policy is a no-op; a change racing resident migration is replayed
// once after that migration reaches a terminal state.
func (self *deviceLocalProvider) SetTransportPolicy(
	targetMode connect.TransportMode,
	modePreferences map[connect.TransportMode]int,
) {
	self.stateLock.Lock()
	if self.closed {
		self.stateLock.Unlock()
		return
	}
	if self.targetMode == targetMode && maps.Equal(self.modePreferences, modePreferences) {
		self.stateLock.Unlock()
		return
	}
	self.targetMode = targetMode
	self.modePreferences = maps.Clone(modePreferences)
	self.transportPolicyVersion += 1
	self.stateLock.Unlock()
	self.requestPlatformTransportMigration(time.Now())
}

func (self *deviceLocalProvider) Client() *connect.Client {
	return self.client
}

// Reports whether the current provider carrier has registered at least one
// route. Migration swaps the carrier under stateLock, so the external status
// read occurs only after the current generation has been captured.
func (self *deviceLocalProvider) IsConnected() bool {
	self.stateLock.Lock()
	if self.closed {
		self.stateLock.Unlock()
		return false
	}
	platformTransport := self.platformTransport
	self.stateLock.Unlock()
	return platformTransport != nil && platformTransport.IsConnected()
}

func (self *deviceLocalProvider) LocalUserNat() *connect.LocalUserNat {
	return self.localUserNat
}

func (self *deviceLocalProvider) SetByJwt(byJwt string) {
	auth := &connect.ClientAuth{
		ByJwt:      byJwt,
		InstanceId: self.instanceId,
		AppVersion: self.appVersion,
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return
	}
	self.auth = auth
	self.authVersion += 1
	if self.clientOob != nil {
		self.clientOob.SetByJwt(byJwt)
	}
	self.platformTransport.SetAuth(auth)
}

func closeMigratablePlatformTransportAndWait(transport migratablePlatformTransport) {
	if transport == nil {
		return
	}
	transport.Close()
	if joiningTransport, ok := transport.(interface {
		CloseAndWait(context.Context) error
	}); ok {
		_ = joiningTransport.CloseAndWait(context.Background())
	}
}

func (self *deviceLocalProvider) closeCompletion() chan struct{} {
	self.closeDoneOnce.Do(func() {
		self.closeDone = make(chan struct{})
	})
	return self.closeDone
}

// Requests callback-safe provider shutdown. The asynchronous join owns all
// migration, transport, client, OOB and NAT completion.
func (self *deviceLocalProvider) Close() {
	done := self.closeCompletion()
	self.closeOnce.Do(func() {
		self.stateLock.Lock()
		self.closed = true
		platformTransport := self.platformTransport
		// the role is joined by the asynchronous close below, since it closes
		// a libp2p host and a listening server
		extender := self.extender
		self.extender = nil
		self.stateLock.Unlock()
		if extender != nil {
			self.migrationWorkers.Add(1)
			go connect.HandleError(func() {
				defer self.migrationWorkers.Done()
				extender.Close()
			})
		}
		if self.cancel != nil {
			self.cancel()
		}
		if platformTransport != nil {
			platformTransport.Close()
		}
		if self.client != nil {
			self.client.Close()
		}
		if self.localUserNat != nil {
			self.localUserNat.Close()
		}
	})
	self.joinOnce.Do(func() {
		go func() {
			self.migrationWorkers.Wait()
			self.stateLock.Lock()
			platformTransport := self.platformTransport
			// read after the migration workers have drained, so a generation
			// built by the last migration is covered too
			directStandbyStrategy := self.directStandbyStrategy
			self.stateLock.Unlock()
			closeMigratablePlatformTransportAndWait(platformTransport)
			if directStandbyStrategy != nil {
				// after the transports that dial with it are joined (J4)
				directStandbyStrategy.Close()
			}
			if self.client != nil {
				_ = self.client.CloseAndWait(context.Background())
			}
			if self.clientOob != nil {
				_ = self.clientOob.CloseAndWait(context.Background())
			}
			if self.localUserNat != nil {
				_ = self.localUserNat.CloseAndWait(context.Background())
			}
			close(done)
		}()
	})
}

// Joins every provider-owned worker. External device owners use this after
// callback-safe Close has detached the provider from routing.
func (self *deviceLocalProvider) CloseAndWait(ctx context.Context) error {
	done := self.closeCompletion()
	self.Close()
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		select {
		case <-done:
			return nil
		default:
			return ctx.Err()
		}
	}
}

func newDeviceClientSettings(
	settings *connect.ClientSettings,
	apiUrl string,
	clientStrategy *connect.ClientStrategy,
) *connect.ClientSettings {
	// Shallow-copy settings (and nested EncryptionSettings) so that
	// filling in defaults never mutates the caller's struct.
	var clientSettings connect.ClientSettings
	if settings != nil {
		clientSettings = *settings
	} else {
		clientSettings = *connect.DefaultClientSettings()
	}
	if clientSettings.EncryptionSettings != nil {
		encryptionSettings := *clientSettings.EncryptionSettings
		clientSettings.EncryptionSettings = &encryptionSettings
	}
	// copy the buffer settings structs too, so a caller-specific budget
	// assignment (the provider pair, the window client stamps) never mutates
	// the caller's structs through the alias. the budget pointers inside
	// carry over, preserving sharing until a caller overwrites them.
	if clientSettings.SendBufferSettings != nil {
		sendBufferSettings := *clientSettings.SendBufferSettings
		clientSettings.SendBufferSettings = &sendBufferSettings
	}
	if clientSettings.ReceiveBufferSettings != nil {
		receiveBufferSettings := *clientSettings.ReceiveBufferSettings
		clientSettings.ReceiveBufferSettings = &receiveBufferSettings
	}
	if clientSettings.ForwardBufferSettings != nil {
		forwardBufferSettings := *clientSettings.ForwardBufferSettings
		clientSettings.ForwardBufferSettings = &forwardBufferSettings
	}
	if clientSettings.ContractManagerSettings != nil {
		contractManagerSettings := *clientSettings.ContractManagerSettings
		clientSettings.ContractManagerSettings = &contractManagerSettings
	}
	if clientSettings.WebRtcSettings != nil {
		webRtcSettings := *clientSettings.WebRtcSettings
		clientSettings.WebRtcSettings = &webRtcSettings
	}
	// A caller may intentionally provide a partial ClientSettings override.
	// Provider memory sizing dereferences these nested settings before
	// connect.NewClient fills defaults, so complete only the missing pieces
	// here while preserving every supplied value and pointer-sharing choice.
	defaults := connect.DefaultClientSettings()
	if clientSettings.SendBufferSettings == nil {
		sendBufferSettings := *defaults.SendBufferSettings
		clientSettings.SendBufferSettings = &sendBufferSettings
	}
	if clientSettings.ReceiveBufferSettings == nil {
		receiveBufferSettings := *defaults.ReceiveBufferSettings
		clientSettings.ReceiveBufferSettings = &receiveBufferSettings
	}
	if clientSettings.WebRtcSettings == nil {
		webRtcSettings := *defaults.WebRtcSettings
		clientSettings.WebRtcSettings = &webRtcSettings
	}

	// Install the default out-of-band peer-key cross-check when none
	// is configured. Callers who want to disable the check can set a
	// no-op NewPeerClientPublicKeyFetcher in their settings.
	if clientSettings.EncryptionSettings != nil &&
		clientSettings.EncryptionSettings.NewPeerClientPublicKeyFetcher == nil {
		clientSettings.EncryptionSettings.NewPeerClientPublicKeyFetcher = func(peerId connect.Id) func(context.Context) ([]byte, error) {
			url := fmt.Sprintf("%s/key/%s", apiUrl, peerId)
			return func(fetchCtx context.Context) ([]byte, error) {
				r, err := connect.HttpGetWithStrategy(
					fetchCtx,
					clientStrategy,
					url,
					"",
					&connect.GetClientKeyResult{},
					connect.NewNoopApiCallback[*connect.GetClientKeyResult](),
				)
				if err != nil {
					return nil, err
				}
				return r.PublicKey, nil
			}
		}
	}

	// Install the signed-identity resolver when none is configured. Unlike the
	// cross-check above this one is enforcing under `EncryptionModeRequired`:
	// it withholds the session cipher until the contract-supplied identity key
	// is corroborated against evidence the operator signed, and a verified
	// disagreement is terminal for that peer. See connect/DESIGNNOTES3.
	if clientSettings.EncryptionSettings != nil &&
		clientSettings.EncryptionSettings.NewPeerClientKeyHistoryFetcher == nil {
		clientSettings.EncryptionSettings.NewPeerClientKeyHistoryFetcher = func(peerId connect.Id) func(context.Context) ([][]byte, error) {
			url := fmt.Sprintf("%s/key/%s/history", apiUrl, peerId)
			return func(fetchCtx context.Context) ([][]byte, error) {
				r, err := connect.HttpGetWithStrategy(
					fetchCtx,
					clientStrategy,
					url,
					"",
					&connect.GetClientKeyHistoryResult{},
					connect.NewNoopApiCallback[*connect.GetClientKeyHistoryResult](),
				)
				if err != nil {
					// an availability failure, never evidence of substitution
					return nil, err
				}
				return r.History, nil
			}
		}
	}

	return &clientSettings
}

// All destination generations reuse the device's durable ratchet. Copy the
// encryption options before attaching it; never mutate shared defaults.
func shareDevicePeerKeyPinStore(client, device *connect.ClientSettings) {
	if device.EncryptionSettings == nil {
		return
	}
	if client.EncryptionSettings == nil {
		client.EncryptionSettings = connect.DefaultEncryptionSettings()
	}
	encryption := *client.EncryptionSettings
	encryption.PeerClientKeyPinStore = device.EncryptionSettings.PeerClientKeyPinStore
	client.EncryptionSettings = &encryption
}

// setExtenderEnabled starts or stops the provider extender role (G2). It is
// called after every provide change and after the setting of F3 changes, and
// is a no-op when the role is already in the requested state or when this
// build carries none (G1).
//
// A role that was asked for and could not be built records why, which the
// status reports as the start error of N3 until the role is asked for again
// or no longer asked for.
func (self *deviceLocalProvider) setExtenderEnabled(enabled bool) {
	self.extenderLock.Lock()
	defer self.extenderLock.Unlock()

	current, closed := func() (*deviceLocalExtender, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.extender, self.closed
	}()
	if closed || !extenderProvideSupported || !extenderProvideRoleEnabled {
		// ios, android and js carry no role at all (G1)
		enabled = false
	}
	if !enabled {
		// nothing is asked for, so nothing failed to start
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.extender = nil
			self.extenderStartError = ""
		}()
		if current != nil {
			current.Close()
		}
		return
	}
	if current != nil {
		return
	}

	settings, err := self.extenderSettings()
	if err != nil {
		self.setExtenderStartError(err)
		return
	}
	extender, err := newDeviceLocalExtender(self.ctx, settings)
	if err != nil {
		self.setExtenderStartError(err)
		return
	}
	installed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		self.extender = extender
		self.extenderStartError = ""
		return true
	}()
	if !installed {
		// the provider closed while the role was being built
		extender.Close()
	}
}

func (self *deviceLocalProvider) setExtenderStartError(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return
	}
	self.extenderStartError = err.Error()
}

// The role's settings for this space and this device (G2, G3), or an error
// when there is nothing to run: a device with no space, or a space with no
// identity to activate under, since an extender whose key changed on every
// launch would be revoked as fast as it activates. The identity is the space's (B1): its persisted `.extender_key`,
// the seed an embedder supplied through the device's key material, or one the
// space generated, which the embedder can read back and keep.
func (self *deviceLocalProvider) extenderSettings() (*deviceLocalExtenderSettings, error) {
	networkSpace := self.networkSpace
	if networkSpace == nil {
		return nil, errors.New("the device has no network space")
	}
	identityKeySeed := networkSpace.extenderIdentityKeySeed()
	if len(identityKeySeed) == 0 {
		return nil, errors.New("the network space has no extender identity")
	}

	// the relay's forward dial is the device's own egress: the extender
	// narrows it by the client's family itself (A7, G2)
	connectSettings := *connect.DefaultConnectSettings()
	if self.clientStrategySettings != nil {
		connectSettings = self.clientStrategySettings.ConnectSettings
	}
	if self.dialContextSettings != nil {
		connectSettings.DialContextSettings = self.dialContextSettings
	}

	settings := &deviceLocalExtenderSettings{
		Log:                    networkSpace.logger(),
		NetworkSpace:           networkSpace,
		AllowedHosts:           networkSpace.extenderAllowedHosts(),
		IdentityKeySeed:        identityKeySeed,
		TcpPort:                connect.ExtenderTcpPort,
		UdpPort:                connect.ExtenderQuicPort,
		DnsPort:                connect.ExtenderDnsPort,
		DnsPrivilegedPort:      extenderDnsPrivilegedPort(),
		DnsTld:                 connect.DefaultExtenderDnsTld,
		ApiUrlV4:               networkSpace.GetApiUrlV4(),
		ApiUrlV6:               networkSpace.GetApiUrlV6(),
		ApiUrl:                 networkSpace.apiUrl,
		HelloUrl:               networkSpace.apiUrl,
		ByJwt:                  self.byJwt,
		ClientStrategySettings: self.clientStrategySettings,
		DialContext:            connectSettings.DialContext,
	}
	if self.extenderSettingsConfigure != nil {
		self.extenderSettingsConfigure(settings)
	}
	return settings, nil
}

// The client jwt of this provider, read at each activation so a refresh is
// picked up by the next one (G3).
func (self *deviceLocalProvider) byJwt() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.auth == nil {
		return ""
	}
	return self.auth.ByJwt
}

// The provider extender status (F3). A provider with no role reports a
// disabled status, carrying why the role did not start when it was asked to
// (N3).
func (self *deviceLocalProvider) extenderProvideStatus() *ExtenderProvideStatus {
	self.stateLock.Lock()
	extender := self.extender
	startError := self.extenderStartError
	self.stateLock.Unlock()
	if extender == nil && startError != "" {
		status := disabledExtenderProvideStatus()
		status.StartError = startError
		return status
	}
	return extender.status()
}

// The relayed traffic of the running role (O2), nil while there is none.
func (self *deviceLocalProvider) extenderStats() *ExtenderStats {
	self.stateLock.Lock()
	extender := self.extender
	self.stateLock.Unlock()
	return extender.stats()
}

// A channel armed at the instant of the read, so a consumer is woken when the
// running role's status changes.
func (self *deviceLocalProvider) extenderStatusUpdate() chan struct{} {
	self.stateLock.Lock()
	extender := self.extender
	self.stateLock.Unlock()
	return extender.statusUpdate()
}
