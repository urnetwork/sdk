package sdk

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	// "github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

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

type deviceLocalProvider struct {
	ctx    context.Context
	cancel context.CancelFunc
	// this is the client for provide
	client       *connect.Client
	clientOob    *connect.ApiOutOfBandControl
	localUserNat *connect.LocalUserNat

	appVersion string
	instanceId connect.Id

	clientStrategy            *connect.ClientStrategy
	platformUrl               string
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

	stateLock         sync.Mutex
	closed            bool
	auth              *connect.ClientAuth
	authVersion       uint64
	platformTransport migratablePlatformTransport
	migrationWorkers  sync.WaitGroup
	closeOnce         sync.Once
	joinOnce          sync.Once
	closeDoneOnce     sync.Once
	closeDone         chan struct{}

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
) *deviceLocalProvider {
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

	resendQueueBudget, receiveQueueBudget := configureDeviceLocalProviderMemory(
		clientSettings,
		providerMemoryTargetByteCount,
	)

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
	platformTransport := connect.NewPlatformTransportWithTargetMode(
		client.Ctx(),
		clientStrategy,
		client.RouteManager(),
		networkSpace.platformUrl,
		auth,
		targetMode,
		platformTransportSettings,
	)

	// This NAT is the local-fallback egress surface: use the explicit
	// provider profile sized from the provider share, so an unbudgeted
	// desktop/server build does not become unbounded, while generic local
	// NAT callers do not inherit phone caps.
	localUserNatSettings := connect.DefaultProviderLocalUserNatSettingsWithMemoryTarget(
		providerMemoryTargetByteCount,
	)
	localUserNatSettings.Log = clientSettings.Log
	localUserNat := connect.NewLocalUserNat(client.Ctx(), clientId.String(), localUserNatSettings)

	provider := &deviceLocalProvider{
		ctx:               providerCtx,
		cancel:            providerCancel,
		client:            client,
		clientOob:         clientOob,
		platformTransport: platformTransport,
		localUserNat:      localUserNat,

		appVersion: appVersion,
		instanceId: instanceId,

		clientStrategy:            clientStrategy,
		platformUrl:               networkSpace.platformUrl,
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
	// the platform asks the client to migrate its transport when the resident
	// is draining (make-before-break, CONNECTDRAIN2.md §3.3)
	client.AddReceiveCallback(provider.handleControlFrames)
	return provider
}

// configureDeviceLocalProviderMemory applies all provider-owned queue and P2P
// budgets in one testable step. Without a target the provider keeps the
// historical wiring and shares the budgets carried in on the copied settings.
func configureDeviceLocalProviderMemory(
	clientSettings *connect.ClientSettings,
	memoryTargetByteCount ByteCount,
) (resendQueueBudget *connect.TransferMemoryBudget, receiveQueueBudget *connect.TransferMemoryBudget) {
	if memoryTargetByteCount <= 0 {
		return
	}

	// Half the provider share is the transfer pair, split 3:4 send:receive;
	// egress NAT flow caps own the other half.
	pairTarget := memoryTargetByteCount / 2
	resendQueueBudget = connect.NewTransferMemoryBudget(max(byteCountFraction(pairTarget, 3, 7), 256*1024))
	receiveQueueBudget = connect.NewTransferMemoryBudget(max(byteCountFraction(pairTarget, 4, 7), 384*1024))
	clientSettings.SendBufferSettings.ResendQueueBudget = resendQueueBudget
	clientSettings.ReceiveBufferSettings.ReceiveQueueBudget = receiveQueueBudget

	// Public P2P connections admit against a dedicated phone-sized pool, not
	// the active transfer receive queue (which is legitimately full precisely
	// when P2P is needed).
	clientSettings.WebRtcSettings.ReceiveBufferSize = deviceLocalP2pReceiveBufferByteCount
	clientSettings.WebRtcSettings.MemoryBudget = deviceLocalWebRtcBudget(memoryTargetByteCount)

	// A trusted ProvideMode_Network peer gets the symmetric selected-peer
	// window from its own bounded two-connection pool. It cannot enlarge or
	// starve the many-peer public pool.
	clientSettings.WebRtcSettings.NetworkPeerReceiveBufferSize =
		deviceLocalNetworkPeerP2pReceiveBufferByteCount
	clientSettings.WebRtcSettings.NetworkPeerMemoryBudget = connect.NewTransferMemoryBudget(
		deviceLocalNetworkPeerP2pConnectionCount * deviceLocalNetworkPeerP2pReceiveBufferByteCount,
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
		next = connect.NewPlatformTransportWithTargetMode(
			self.client.Ctx(),
			self.clientStrategy,
			self.client.RouteManager(),
			self.platformUrl,
			auth,
			targetMode,
			platformTransportSettings,
		)
	}
	brokeBeforeMake := false
	if nextPlatform, ok := next.(*connect.PlatformTransport); ok {
		self.stateLock.Lock()
		previous := self.platformTransport
		self.stateLock.Unlock()
		if previousPlatform, ok := previous.(*connect.PlatformTransport); ok &&
			!nextPlatform.CanMakeBeforeBreakFrom(previousPlatform) {
			// A second full H3 working set would escape the shared memory cap.
			// H1 transitions use Connect's bounded handoff and keep the old route;
			// only a budget-blocked H3-to-H3-family transition breaks first.
			closeMigratablePlatformTransportAndWait(previous)
			brokeBeforeMake = true
		}
	}
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
		self.stateLock.Unlock()
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
			self.stateLock.Unlock()
			closeMigratablePlatformTransportAndWait(platformTransport)
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

	return &clientSettings
}
