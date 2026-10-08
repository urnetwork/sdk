//go:build !sdk_mobile_bind

package sdk

import (
	"context"
	"net"
	"sort"
	"sync"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// THE VPN, MADE SMALL ENOUGH TO CARRY ONE APP'S CONNECTIONS AND NOTHING ELSE.
//
// It is the same four parts DeviceLocal runs for the VPN, minus the operating system: the
// provider generator (find-providers2 on the operator, the window clients it mints with
// AuthNetworkClient), a RemoteUserNatMultiClient that sends IP packets to the exit providers it
// picked, and connect's gvisor Tun turning those packets back into sockets. SimClient
// (sim_device.go) proves the composition for simulations; this is that composition with the
// settings a real user's traffic needs. There is no TUN adapter, no route table change and no
// service, so it needs no administrator: the only packets in it are the ones this process makes
// by dialling [messageTunnel.DialContext].
//
// TWO SETTINGS ARE THE POINT, AND BOTH ARE NAMED HERE RATHER THAN INHERITED.
//
//   - Per-peer encryption OPPORTUNISTIC, not REQUIRED: the owner's ruling of 2026-10-04,
//     "Opportunistic + provider fix" ([messageTunnelClientSettings]). Every window client asks its
//     exit for a connect per-peer session (transfer_encrypt.go): TLS 1.3 preferring the
//     X25519MLKEM768 hybrid, and the exit's identity proof checked against the identity key the
//     operator names for it. What that hides, and when, is below.
//   - AllowDirect false: no peer-to-peer link from this device to the exit, so traffic goes via the
//     operator's relay and the exit never learns this device's address (sdk.go: "setting this to
//     true exposes the real source IP to the provider").
//
// WHAT THE OPERATOR'S RELAY CAN READ. In every case the content is TLS to the message server's
// pinned key (message_route.go), and the relay sees sizes and timing, because the per-peer layer
// does not pad. Beyond that, it depends on the exit's session:
//
//   - After the session completes: nothing. Not the destination address, the port, or the TCP and
//     TLS headers inside.
//   - Before it completes: every packet, because OPPORTUNISTIC does not hold them. On 2026-10-04
//     every window to an exit that answered sent 3 to 6 writes this way, in the 0.55 to 0.62 s
//     between its ClientHello and the session sealing. They opened the TCP connection to the
//     server's endpoint, and in four windows of five carried its TLS ClientHello too, so the
//     relay could read that destination and port.
//   - With an exit that never answers: every packet, as the pre-merge alpha sent them to every
//     exit. On 2026-10-04 that was nearly every exit on the beta (ledger 278).
//
// THE TRADE. REQUIRED, which the profile's PostQuantumEncryption turns on, holds every packet until
// its exit seals, so the relay never reads a destination. On 2026-10-04 it also made the first
// Hello take minutes or fail, because 1 exit in 71 answered (ledger 278). OPPORTUNISTIC is as fast
// as the alpha and seals with every exit that answers, but it defeats only an operator that reads,
// not one that interferes. A completed session is the same TLS and the same identity proof as under
// REQUIRED. What REQUIRED adds, and this lacks:
//
//   - The hold before the session completes: the fail-closed entry gate in SendSequence.Pack
//     (transfer.go).
//   - The signed key-history hold: the cipher waits until the exit's identity key is corroborated
//     against the operator's signed registration history (keyHistoryRequiredWithLock,
//     transfer_key_history_session.go).
//   - The gate that drops a plaintext application frame from an exit a session is expected with
//     (ReceiveSequence.receiveHead, transfer.go). Without it, a plaintext packet the relay injects
//     in a sealed exit's name is delivered to this app's sockets. And a relay gets plaintext by
//     dropping the handshake, or, once a session has sealed, by forging the nack that
//     handleUnknownWrapNack (transfer_encrypt.go) answers by demoting it. This app cannot tell.
//
// Reconsider REQUIRED once most exits answer.
//
// WHAT IT DOES NOT DO: multi-hop. The window is whatever find-providers2 returns, and the deployed
// operator returns no intermediary ids today (ledger 268), so a route is one exit provider behind
// the operator's relay.
type messageTunnel struct {
	ctx    context.Context
	cancel context.CancelFunc

	multiClient *connect.RemoteUserNatMultiClient
	tun         *connect.Tun

	bridgeWg sync.WaitGroup
}

type messageTunnelConfig struct {
	ByClientJwt string
	ClientId    connect.Id
	ApiUrl      string
	PlatformUrl string
	AppVersion  string
}

func newMessageTunnel(ctx context.Context, config *messageTunnelConfig) (*messageTunnel, error) {
	cancelCtx, cancel := context.WithCancel(ctx)

	clientStrategy := connect.NewClientStrategyWithDefaults(cancelCtx)
	clientId := config.ClientId
	generator := connect.NewApiMultiClientGenerator(
		cancelCtx,
		[]*connect.ProviderSpec{{BestAvailable: true}},
		clientStrategy,
		// exclude self
		[]connect.Id{clientId},
		config.ApiUrl,
		config.ByClientJwt,
		config.PlatformUrl,
		"urmessage",
		"",
		config.AppVersion,
		&clientId,
		func() *connect.ClientSettings {
			return messageTunnelClientSettings(config.ApiUrl, clientStrategy)
		},
		connect.DefaultApiMultiClientGeneratorSettings(),
	)
	return startMessageTunnel(cancelCtx, cancel, generator, clientId)
}

// messageTunnelClientSettings are the settings of every window client the tunnel mints: the
// device's own window-client settings, which wire the operator's key api (/key/<id> and its signed
// history), with the per-peer encryption mode OPPORTUNISTIC. Traffic to an exit that answers the
// per-peer handshake is sealed from the moment its session completes; an exit that never answers
// is reached unsealed at this layer instead of not at all. The owner's ruling of 2026-10-04
// ("Opportunistic + provider fix"); the type comment above has the trade.
func messageTunnelClientSettings(apiUrl string, clientStrategy *connect.ClientStrategy) *connect.ClientSettings {
	settings := connect.DefaultClientSettings()
	if settings.EncryptionSettings == nil {
		settings.EncryptionSettings = connect.DefaultEncryptionSettings()
	}
	settings.EncryptionSettings.Mode = connect.EncryptionModeOpportunistic
	return newDeviceClientSettings(settings, apiUrl, clientStrategy)
}

// messageTunnelMultiClientSettings carry the window's performance profile. PostQuantumEncryption
// stays false although the sessions run: connect reads it as "REQUIRED" and would override
// [messageTunnelClientSettings]' mode on every window client (newMultiClientChannel), which is the
// regression of ledger 278. The key exchange does not depend on it: every per-peer session
// prefers the X25519MLKEM768 hybrid whatever the mode.
func messageTunnelMultiClientSettings() *connect.MultiClientSettings {
	multiClientSettings := connect.DefaultMultiClientSettings()
	multiClientSettings.DefaultPerformanceProfile = &connect.PerformanceProfile{
		PostQuantumEncryption: false,
		AllowDirect:           false,
	}
	return multiClientSettings
}

// startMessageTunnel runs the tunnel over a provider generator: the operator's in
// [newMessageTunnel], an exit in this process in the tests.
func startMessageTunnel(
	cancelCtx context.Context,
	cancel context.CancelFunc,
	generator connect.MultiClientGenerator,
	clientId connect.Id,
) (*messageTunnel, error) {
	tun, err := connect.CreateTun(cancelCtx, connect.DefaultTunSettings())
	if err != nil {
		cancel()
		return nil, err
	}

	// multi client -> tun. Tun.Write copies into the gvisor stack, so the packet is not retained
	receivePacket := func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		tun.Write(packet)
	}
	multiClient := connect.NewRemoteUserNatMultiClient(
		cancelCtx,
		generator,
		receivePacket,
		protocol.ProvideMode_Public,
		messageTunnelMultiClientSettings(),
	)

	self := &messageTunnel{
		ctx:         cancelCtx,
		cancel:      cancel,
		multiClient: multiClient,
		tun:         tun,
	}

	// tun -> multi client, with DeviceLocal.SendPacket's ownership rule: SendPacket consumes the
	// buffer on success and leaves it to the caller on failure (see SimClient's bridge for the
	// double free that returning it unconditionally caused)
	source := connect.SourceId(clientId)
	self.bridgeWg.Add(1)
	go connect.HandleError(func() {
		defer self.bridgeWg.Done()
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

	return self, nil
}

// DialContext dials through the tunnel: client -> operator relay (sealed once the exit has
// answered the per-peer handshake) -> exit provider -> destination.
func (self *messageTunnel) DialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	return self.tun.DialContext(ctx, network, address)
}

// window is what the tunnel's window holds right now: how many exit providers are routing, and
// the countries they are in.
func (self *messageTunnel) window() (providers int, countries []string) {
	_, providerEvents := self.multiClient.Monitor().Events()
	seen := map[string]bool{}
	for _, event := range providerEvents {
		if event.State != connect.ProviderStateAdded {
			continue
		}
		providers += 1
		if event.Location != nil && event.Location.Country != "" && !seen[event.Location.Country] {
			seen[event.Location.Country] = true
			countries = append(countries, event.Location.Country)
		}
	}
	sort.Strings(countries)
	return providers, countries
}

func (self *messageTunnel) Close() {
	self.tun.Close() // unblocks ReadBatch, so the bridge exits
	self.bridgeWg.Wait()
	self.multiClient.Close()
	self.cancel()
}
