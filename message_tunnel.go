package sdk

import (
	"context"
	"net"
	"sort"
	"sync"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
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
//   - PostQuantumEncryption: every window client runs connect's per-peer sessions REQUIRED
//     (ip_remote_multi_client.go, the pqe branch). The operator relays this app's packets to the
//     exit and, with this on, relays them sealed: it does not see the destination address, the
//     port or anything inside. The VPN leaves this off by default; this does not, because the
//     whole purpose of routing a messenger through the mesh is that the operator learns less.
//   - AllowDirect false: no peer-to-peer link from this device to the exit, so traffic goes via the
//     operator's relay and the exit never learns this device's address (sdk.go: "setting this to
//     true exposes the real source IP to the provider").
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
			// the device's own window-client settings, which wire the operator's key api as the
			// identity cross-check the REQUIRED sessions are verified against
			return newDeviceClientSettings(connect.DefaultClientSettings(), config.ApiUrl, clientStrategy)
		},
		connect.DefaultApiMultiClientGeneratorSettings(),
	)

	tun, err := connect.CreateTun(cancelCtx, connect.DefaultTunSettings())
	if err != nil {
		cancel()
		return nil, err
	}

	multiClientSettings := connect.DefaultMultiClientSettings()
	multiClientSettings.DefaultPerformanceProfile = &connect.PerformanceProfile{
		PostQuantumEncryption: true,
		AllowDirect:           false,
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
		multiClientSettings,
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

// DialContext dials through the tunnel: client -> operator relay (sealed) -> exit provider ->
// destination.
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
