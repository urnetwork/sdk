package sdk

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	// "github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
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
	ctx context.Context
	// this is the client for provide
	client       *connect.Client
	localUserNat *connect.LocalUserNat

	appVersion string
	instanceId connect.Id

	clientStrategy            *connect.ClientStrategy
	platformUrl               string
	platformTransportSettings *connect.PlatformTransportSettings

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
	newPlatformTransport func(auth *connect.ClientAuth) migratablePlatformTransport

	stateLock         sync.Mutex
	auth              *connect.ClientAuth
	authVersion       uint64
	platformTransport migratablePlatformTransport
}

func newDeviceLocalProviderWithOverrides(
	ctx context.Context,
	networkSpace *NetworkSpace,
	byJwt string,
	appVersion string,
	instanceId connect.Id,
	settings *connect.ClientSettings,
	clientId connect.Id,
) *deviceLocalProvider {
	apiUrl := networkSpace.apiUrl
	clientStrategy := networkSpace.clientStrategy

	clientOob := connect.NewApiOutOfBandControl(ctx, clientStrategy, byJwt, apiUrl)

	clientSettings := newDeviceClientSettings(settings, apiUrl, clientStrategy)
	// the provider always enables the e2e encryption sessions: the responder
	// serves plain and e2e peers seamlessly (a session only forms when an
	// initiator starts a handshake), and every enabled provider grows the
	// e2e-capable pool for pqe initiators
	if clientSettings.EncryptionSettings == nil {
		clientSettings.EncryptionSettings = connect.DefaultEncryptionSettings()
	}
	clientSettings.EncryptionSettings.Encrypt = true

	client := connect.NewClient(
		ctx,
		clientId,
		clientOob,
		clientSettings,
	)

	auth := &connect.ClientAuth{
		ByJwt:      byJwt,
		InstanceId: instanceId,
		AppVersion: appVersion,
	}
	platformTransportSettings := connect.DefaultPlatformTransportSettings()
	platformTransportSettings.Log = clientSettings.Log
	platformTransport := connect.NewPlatformTransport(
		client.Ctx(),
		clientStrategy,
		client.RouteManager(),
		networkSpace.platformUrl,
		auth,
		platformTransportSettings,
	)

	// This NAT is the provider egress surface: use the explicit provider
	// profile so an unbudgeted desktop/server build does not become
	// unbounded, while generic local NAT callers do not inherit phone caps.
	localUserNatSettings := connect.DefaultProviderLocalUserNatSettings()
	localUserNatSettings.Log = clientSettings.Log
	localUserNat := connect.NewLocalUserNat(client.Ctx(), clientId.String(), localUserNatSettings)

	provider := &deviceLocalProvider{
		ctx:               ctx,
		client:            client,
		platformTransport: platformTransport,
		localUserNat:      localUserNat,

		appVersion: appVersion,
		instanceId: instanceId,

		clientStrategy:            clientStrategy,
		platformUrl:               networkSpace.platformUrl,
		platformTransportSettings: platformTransportSettings,
		migrateConnectTimeout:     platformTransportMigrateConnectTimeout,
		migrateMaxScheduleDelay:   platformTransportMigrateMaxScheduleDelay,
		auth:                      auth,
	}
	// the platform asks the client to migrate its transport when the resident
	// is draining (make-before-break, CONNECTDRAIN2.md §3.3)
	client.AddReceiveCallback(provider.handleControlFrames)
	return provider
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
		if self.migrating.CompareAndSwap(false, true) {
			go connect.HandleError(func() {
				defer self.migrating.Store(false)
				self.migratePlatformTransport(migrateTime)
			})
		}
	}
}

// migratePlatformTransport performs make-before-break at `migrateTime`: build
// a replacement platform transport while the current one keeps carrying
// traffic, wait for the replacement to connect (bounded), then close the old
// transport so its routes drop and traffic continues over the replacement.
// On timeout the replacement is closed and the old transport stays: the
// draining server evicts it, and the reconnect falls back to the drain excuse
// path (CONNECTDRAIN2.md §3.3).
func (self *deviceLocalProvider) migratePlatformTransport(migrateTime time.Time) {
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
			return
		case <-timer.C:
		}
	}

	auth, authVersion := func() (*connect.ClientAuth, uint64) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		auth := *self.auth
		return &auth, self.authVersion
	}()
	var next migratablePlatformTransport
	if self.newPlatformTransport != nil {
		next = self.newPlatformTransport(auth)
	} else {
		next = connect.NewPlatformTransport(
			self.client.Ctx(),
			self.clientStrategy,
			self.client.RouteManager(),
			self.platformUrl,
			auth,
			self.platformTransportSettings,
		)
	}

	connectEndTime := time.Now().Add(self.migrateConnectTimeout)
	for {
		notify := next.ConnectedNotify()
		if next.IsConnected() {
			break
		}
		if connectEndTime.Before(time.Now()) {
			// the replacement did not come up; keep the old transport
			next.Close()
			return
		}
		select {
		case <-self.ctx.Done():
			next.Close()
			return
		case <-notify:
		case <-time.After(1 * time.Second):
		}
	}

	var previous migratablePlatformTransport
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		// Token refresh can race replacement construction/connection. Reapply
		// the current immutable auth while holding the same lock used by
		// SetByJwt and the swap; a later refresh therefore updates next, while
		// an earlier one cannot be overwritten by the captured stale auth.
		if authVersion != self.authVersion {
			next.SetAuth(self.auth)
		}
		previous = self.platformTransport
		self.platformTransport = next
	}()
	if previous != nil {
		previous.Close()
	}
}

func (self *deviceLocalProvider) Client() *connect.Client {
	return self.client
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
	self.auth = auth
	self.authVersion += 1
	self.platformTransport.SetAuth(auth)
}

func (self *deviceLocalProvider) Close() {
	self.client.Close()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.platformTransport.Close()
	}()
	self.localUserNat.Close()
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
