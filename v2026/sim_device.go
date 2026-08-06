package sdk

// simulation surfaces for headless providers and clients.
//
// These compose the same connect components the full `DeviceLocal` device
// uses — client, platform transport, `LocalUserNat`/`RemoteUserNatProvider`
// on the provider side, `ApiMultiClientGenerator`/`RemoteUserNatMultiClient`
// plus a gvisor `Tun` on the client side — without the mobile device
// scaffolding, so a single process can host large fleets of them. They are
// used by the sim-latency load/latency simulation (server/connect/sim-latency)
// and carry the simulation overrides that tool needs: per-identity extra
// headers (fake forwarded-for addresses), a custom dial context (network
// impairment injection), and disabled egress security rules (the simulated
// origin site is on a private address).
//
// Everything here is safe for concurrent use unless noted.

import (
	"context"
	"net"
	"net/http"
	"sync"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// SimProviderConfig configures a headless egress provider.
//
// gomobile:ignore
type SimProviderConfig struct {
	ApiUrl      string
	PlatformUrl string
	ByJwt       string
	ClientId    connect.Id
	InstanceId  connect.Id
	AppVersion  string
	// applied to every api request and platform ws dial from this provider
	// (e.g. `X-UR-Forwarded-For` to present a per-provider fake address)
	ExtraHeaders http.Header
	// wraps every plain-scheme (http/ws) connection from this provider.
	// nil uses the standard dialer. used to inject simulated network
	// impairment under the platform transport
	DialContext func(ctx context.Context, network string, addr string) (net.Conn, error)
	// allow egress to loopback/private destinations (the simulated origin
	// site is on a local address, which the default policy blocks)
	DisableSecurityPolicy bool
	// caps the egress NAT's concurrent flows (tcp and udp each), modeling a
	// device connection limit (ulimit). Over the cap the NAT admits the new
	// flow and lru-evicts the idle-most established flow (see the
	// `LocalUserNat` flow limits). 0 = unlimited.
	MaxConcurrentFlows int
	// nil uses `connect.DefaultClientSettings()`
	ClientSettings *connect.ClientSettings
	Log            connect.Logger
}

// SimProvider is a headless egress provider: a connect client providing
// public egress through a real `LocalUserNat`, connected to the platform
// over a websocket transport. `SetConnected(false)`/`SetConnected(true)`
// model provider churn: the transport is torn down and later re-established
// (a fresh connection, as a real provider reconnect would be) while the
// client and its provide state persist.
//
// gomobile:ignore
type SimProvider struct {
	ctx    context.Context
	cancel context.CancelFunc

	client         *connect.Client
	localUserNat   *connect.LocalUserNat
	remoteUserNat  *connect.RemoteUserNatProvider
	clientStrategy *connect.ClientStrategy

	platformUrl               string
	auth                      *connect.ClientAuth
	platformTransportSettings *connect.PlatformTransportSettings

	stateLock         sync.Mutex
	platformTransport *connect.PlatformTransport
}

// NewSimProvider builds the provider and connects its transport.
//
// gomobile:ignore
func NewSimProvider(ctx context.Context, config *SimProviderConfig) *SimProvider {
	cancelCtx, cancel := context.WithCancel(ctx)

	log := config.Log
	if log == nil {
		log = connect.NewNoopLogger()
	}

	clientStrategySettings := connect.DefaultClientStrategySettings()
	clientStrategySettings.Log = log
	// only the direct dialer; the resilient dialers are unnecessary (and an
	// extra failure mode) against a local server
	clientStrategySettings.EnableNormal = true
	clientStrategySettings.EnableResilient = false
	clientStrategySettings.ExtraHeaders = config.ExtraHeaders
	if config.DialContext != nil {
		clientStrategySettings.ConnectSettings.DialContextSettings = &connect.DialContextSettings{
			DialContext: config.DialContext,
		}
	}
	clientStrategy := connect.NewClientStrategy(cancelCtx, clientStrategySettings)

	clientSettings := config.ClientSettings
	if clientSettings == nil {
		clientSettings = connect.DefaultClientSettings()
	}
	clientSettings.Log = log

	clientOob := connect.NewApiOutOfBandControl(cancelCtx, clientStrategy, config.ByJwt, config.ApiUrl)
	client := connect.NewClient(cancelCtx, config.ClientId, clientOob, clientSettings)

	// This NAT egresses untrusted remote traffic, so use the explicit
	// provider profile even though simulations normally have no device memory
	// budget installed.
	localUserNatSettings := connect.DefaultProviderLocalUserNatSettings()
	localUserNatSettings.Log = log
	if 0 < config.MaxConcurrentFlows {
		// the provider profile leaves flow counts unlimited when no device
		// memory budget is installed (the sim case); an explicit cap applies
		// regardless
		localUserNatSettings.TcpBufferSettings.GlobalLimit = config.MaxConcurrentFlows
		localUserNatSettings.UdpBufferSettings.GlobalLimit = config.MaxConcurrentFlows
	}
	localUserNat := connect.NewLocalUserNat(client.Ctx(), config.ClientId.String(), localUserNatSettings)

	remoteUserNatProviderSettings := connect.DefaultRemoteUserNatProviderSettings()
	if config.DisableSecurityPolicy {
		remoteUserNatProviderSettings.SecurityPolicyGenerator = connect.DisableSecurityPolicyWithStats
	}
	remoteUserNat := connect.NewRemoteUserNatProvider(client, localUserNat, remoteUserNatProviderSettings)

	client.ContractManager().SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Network: true,
		protocol.ProvideMode_Public:  true,
	})

	platformTransportSettings := connect.DefaultPlatformTransportSettings()
	platformTransportSettings.Log = log

	provider := &SimProvider{
		ctx:            cancelCtx,
		cancel:         cancel,
		client:         client,
		localUserNat:   localUserNat,
		remoteUserNat:  remoteUserNat,
		clientStrategy: clientStrategy,
		platformUrl:    config.PlatformUrl,
		auth: &connect.ClientAuth{
			ByJwt:      config.ByJwt,
			InstanceId: config.InstanceId,
			AppVersion: config.AppVersion,
		},
		platformTransportSettings: platformTransportSettings,
	}
	provider.SetConnected(true)
	return provider
}

func (self *SimProvider) Client() *connect.Client {
	return self.client
}

func (self *SimProvider) LocalUserNat() *connect.LocalUserNat {
	return self.localUserNat
}

// IsConnected reports whether the platform transport exists and has routes
// registered (i.e. the provider is live on the platform)
func (self *SimProvider) IsConnected() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.platformTransport != nil && self.platformTransport.IsConnected()
}

// SetConnected connects or disconnects the platform transport. Disconnect
// closes the transport entirely (no auto-reconnect), so the provider is
// offline until the next `SetConnected(true)`, which dials a fresh
// connection.
func (self *SimProvider) SetConnected(connected bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if connected {
		if self.platformTransport == nil {
			// h1 only: the sim platform is a local ws:// url
			self.platformTransport = connect.NewPlatformTransportWithTargetMode(
				self.client.Ctx(),
				self.clientStrategy,
				self.client.RouteManager(),
				self.platformUrl,
				self.auth,
				connect.TransportModeH1,
				self.platformTransportSettings,
			)
		}
	} else {
		if self.platformTransport != nil {
			self.platformTransport.Close()
			self.platformTransport = nil
		}
	}
}

func (self *SimProvider) Close() {
	self.SetConnected(false)
	self.remoteUserNat.Close()
	self.localUserNat.Close()
	self.client.Close()
	self.cancel()
}

// SimClientConfig configures a headless client.
//
// gomobile:ignore
type SimClientConfig struct {
	ApiUrl      string
	PlatformUrl string
	// a network-level client jwt. the multi client derives per-window
	// clients from it via the api (`AuthNetworkClient`)
	ByJwt             string
	ClientId          connect.Id
	AppVersion        string
	DeviceDescription string
	DeviceSpec        string
	// the provider specs to egress through (e.g. the sim country location)
	Specs []*connect.ProviderSpec
	// applied to every api request and platform ws dial from this client
	ExtraHeaders http.Header
	// wraps every plain-scheme (http/ws) connection from this client
	DialContext func(ctx context.Context, network string, addr string) (net.Conn, error)
	// disable local egress inspection (the simulated origin site is on a
	// private address)
	DisableSecurityPolicy bool
	// nil uses `connect.DefaultTunSettings()`
	TunSettings *connect.TunSettings
	Log         connect.Logger
}

// SimClient is a headless client: a `RemoteUserNatMultiClient` over
// api-discovered providers (the same generator the device uses), bridged to
// a gvisor `Tun` so tcp/udp flows can be driven at the socket level with
// `DialContext`.
//
// gomobile:ignore
type SimClient struct {
	ctx    context.Context
	cancel context.CancelFunc

	clientStrategy *connect.ClientStrategy
	multiClient    *connect.RemoteUserNatMultiClient
	tun            *connect.Tun

	bridgeWg sync.WaitGroup
}

// NewSimClient builds the client. The multi client begins discovering
// providers (via find-providers2) immediately.
//
// gomobile:ignore
func NewSimClient(ctx context.Context, config *SimClientConfig) (*SimClient, error) {
	cancelCtx, cancel := context.WithCancel(ctx)

	log := config.Log
	if log == nil {
		log = connect.NewNoopLogger()
	}

	clientStrategySettings := connect.DefaultClientStrategySettings()
	clientStrategySettings.Log = log
	clientStrategySettings.EnableNormal = true
	clientStrategySettings.EnableResilient = false
	clientStrategySettings.ExtraHeaders = config.ExtraHeaders
	if config.DialContext != nil {
		clientStrategySettings.ConnectSettings.DialContextSettings = &connect.DialContextSettings{
			DialContext: config.DialContext,
		}
	}
	clientStrategy := connect.NewClientStrategy(cancelCtx, clientStrategySettings)

	generator := connect.NewApiMultiClientGenerator(
		cancelCtx,
		config.Specs,
		clientStrategy,
		// exclude self
		[]connect.Id{config.ClientId},
		config.ApiUrl,
		config.ByJwt,
		config.PlatformUrl,
		config.DeviceDescription,
		config.DeviceSpec,
		config.AppVersion,
		&config.ClientId,
		connect.DefaultClientSettings,
		connect.DefaultApiMultiClientGeneratorSettings(),
	)

	tunSettings := config.TunSettings
	if tunSettings == nil {
		tunSettings = connect.DefaultTunSettings()
	}
	tunSettings.Log = log
	tun, err := connect.CreateTun(cancelCtx, tunSettings)
	if err != nil {
		cancel()
		return nil, err
	}

	multiClientSettings := connect.DefaultMultiClientSettings()
	multiClientSettings.Log = log
	if config.DisableSecurityPolicy {
		multiClientSettings.SecurityPolicyGenerator = connect.DisableSecurityPolicyWithStats
	}

	// multi client -> tun. Tun.Write copies into the gvisor stack, so the
	// packet is not retained past the callback
	receivePacket := func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		tun.Write(packet)
	}

	multiClient := connect.NewRemoteUserNatMultiClient(
		cancelCtx,
		generator,
		receivePacket,
		protocol.ProvideMode_Public,
		multiClientSettings,
	)

	simClient := &SimClient{
		ctx:            cancelCtx,
		cancel:         cancel,
		clientStrategy: clientStrategy,
		multiClient:    multiClient,
		tun:            tun,
	}

	// tun -> multi client. SendPacket consumes the buffer on success (the send
	// path returns it once the frame is handed off) and leaves it to the caller
	// only on failure, so the bridge returns it exactly when the send fails —
	// mirroring DeviceLocal.SendPacket. Returning unconditionally double-frees
	// the pooled buffer on every successful send, corrupting the stream. A
	// blocking send (-1) keeps the source lossless, and unblocks when the tun
	// closes.
	source := connect.SourceId(config.ClientId)
	simClient.bridgeWg.Add(1)
	go connect.HandleError(func() {
		defer simClient.bridgeWg.Done()
		packets := make([][]byte, 64)
		for {
			n, err := tun.ReadBatch(packets)
			if err != nil {
				return
			}
			for _, packet := range packets[:n] {
				if !multiClient.SendPacket(source, protocol.ProvideMode_Network, packet, -1) {
					connect.MessagePoolReturn(packet)
				}
			}
		}
	})

	return simClient, nil
}

// DialContext dials through the tunnel: the connection's bytes traverse
// client -> platform -> provider egress to the destination
func (self *SimClient) DialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	return self.tun.DialContext(ctx, network, address)
}

func (self *SimClient) MultiClient() *connect.RemoteUserNatMultiClient {
	return self.multiClient
}

func (self *SimClient) Close() {
	self.tun.Close() // unblocks ReadBatch -> bridge goroutine exits
	self.bridgeWg.Wait()
	self.multiClient.Close()
	self.cancel()
}
