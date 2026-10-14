package sdk

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// reraceSdkExit is one in-process provider of the dial-failure test: the
// production provider NAT and RemoteUserNatProvider behind a synthetic socket
// dialer (see newSyntheticExitProvider), at a platform tier.
type reraceSdkExit struct {
	name string
	tier int
	*syntheticExitProvider
}

func (self *reraceSdkExit) destination() connect.MultiHopId {
	return connect.RequireMultiHopId(self.client.ClientId())
}

// reraceSdkGenerator offers the quality window every exit at its tier, never
// one the window already holds, and wires each window client to the exit it
// was minted for over bounded in-memory routes, the way
// syntheticProviderGenerator wires its single exit. The speed window is
// offered nothing: the test connects over https, which the quality window
// carries.
type reraceSdkGenerator struct {
	deviceSettings *DeviceLocalSettings
	exits          []*reraceSdkExit

	mutex       sync.Mutex
	clientExits map[connect.Id]*reraceSdkExit
	unsubs      map[connect.Id]func()
}

func (self *reraceSdkGenerator) NextDestinations(
	count int,
	excludeDestinations []connect.MultiHopId,
	rankMode string,
) (map[connect.MultiHopId]connect.DestinationStats, error) {
	next := map[connect.MultiHopId]connect.DestinationStats{}
	if rankMode != connect.WindowTypeQuality.RankMode() {
		return next, nil
	}
	self.mutex.Lock()
	defer self.mutex.Unlock()
	held := map[*reraceSdkExit]bool{}
	for _, exit := range self.clientExits {
		held[exit] = true
	}
	for _, exit := range self.exits {
		if count <= len(next) || held[exit] || slices.Contains(excludeDestinations, exit.destination()) {
			continue
		}
		next[exit.destination()] = connect.DestinationStats{Tier: exit.tier}
	}
	return next, nil
}

func (self *reraceSdkGenerator) NewClientArgsForDestination(
	destination connect.MultiHopId,
) (*connect.MultiClientGeneratorClientArgs, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	for _, exit := range self.exits {
		if exit.destination() == destination {
			args := &connect.MultiClientGeneratorClientArgs{ClientId: connect.NewId()}
			self.clientExits[args.ClientId] = exit
			return args, nil
		}
	}
	return nil, errors.New("unknown destination")
}

func (*reraceSdkGenerator) NewClientArgs() (*connect.MultiClientGeneratorClientArgs, error) {
	return nil, errors.New("client args are minted per destination")
}

func (self *reraceSdkGenerator) RemoveClientArgs(args *connect.MultiClientGeneratorClientArgs) {
	self.mutex.Lock()
	unsub := self.unsubs[args.ClientId]
	delete(self.unsubs, args.ClientId)
	delete(self.clientExits, args.ClientId)
	self.mutex.Unlock()
	if unsub != nil {
		unsub()
	}
}

func (self *reraceSdkGenerator) RemoveClientWithArgs(_ *connect.Client, args *connect.MultiClientGeneratorClientArgs) {
	self.RemoveClientArgs(args)
}

func (self *reraceSdkGenerator) NewClientSettings() *connect.ClientSettings {
	return (&syntheticProviderGenerator{deviceSettings: self.deviceSettings}).NewClientSettings()
}

func (self *reraceSdkGenerator) NewClient(
	ctx context.Context,
	args *connect.MultiClientGeneratorClientArgs,
	clientSettings *connect.ClientSettings,
) (*connect.Client, error) {
	self.mutex.Lock()
	exit := self.clientExits[args.ClientId]
	self.mutex.Unlock()
	if exit == nil {
		return nil, errors.New("client args were not minted for a destination")
	}
	client := connect.NewClient(ctx, args.ClientId, connect.NewNoContractClientOob(), clientSettings)
	toProvider := make(chan []byte)
	toClient := make(chan []byte)
	clientSend := connect.NewSendGatewayTransport()
	clientReceive := connect.NewReceiveGatewayTransport()
	client.RouteManager().UpdateTransport(clientSend, []connect.Route{toProvider})
	client.RouteManager().UpdateTransport(clientReceive, []connect.Route{toClient})
	client.ContractManager().AddNoContractPeer(exit.client.ClientId())

	providerSend := connect.NewSendClientTransport(connect.DestinationId(client.ClientId()))
	providerReceive := connect.NewReceiveGatewayTransport()
	exit.client.RouteManager().UpdateTransport(providerReceive, []connect.Route{toProvider})
	exit.client.RouteManager().UpdateTransport(providerSend, []connect.Route{toClient})
	exit.client.ContractManager().AddNoContractPeer(client.ClientId())

	var once sync.Once
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.unsubs[args.ClientId] = func() {
		once.Do(func() {
			client.RouteManager().RemoveTransport(clientSend)
			client.RouteManager().RemoveTransport(clientReceive)
			exit.client.RouteManager().RemoveTransport(providerReceive)
			exit.client.RouteManager().RemoveTransport(providerSend)
			client.Cancel()
		})
	}
	return client, nil
}

func (*reraceSdkGenerator) FixedDestinationSize() (int, bool) {
	return 0, false
}

// exitOf is the exit a window client was minted for, nil once released.
func (self *reraceSdkGenerator) exitOf(clientId connect.Id) *reraceSdkExit {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.clientExits[clientId]
}

// RR-13. Through the device: a destination provider A cannot dial is reached
// through provider B within about one SYN retransmit, and the app's TCP
// connect succeeds. A and B run the production provider NAT and provider;
// A's dialer fails every dial (a capacity-class error, as a provider out of
// sockets fails), so A answers the SYN with its dial-failure signal, and B's
// reaches a loopback echo server. A is the best tier, so the SYN goes to A
// alone; the signal unbinds the flow and the stack's first retransmit (1s)
// races B. Without the signal reaching the client the flow would wait for the
// 3s silence inference instead, so connecting before it is the proof.
func TestDeviceLocalDialFailureReracesConnectToAnotherExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var echo sync.WaitGroup
	echo.Add(1)
	go func() {
		defer echo.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			echo.Add(1)
			go func() {
				defer echo.Done()
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()
	defer func() {
		// after the exits close, which ends their upstream sockets
		listener.Close()
		echo.Wait()
	}()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.Verbose = false
	settings.AllowProvider = false

	exitA := &reraceSdkExit{name: "a", tier: 0, syntheticExitProvider: newSyntheticExitProvider(ctx, settings, map[int]string{})}
	defer exitA.Close()
	exitB := &reraceSdkExit{name: "b", tier: 1, syntheticExitProvider: newSyntheticExitProvider(ctx, settings, map[int]string{
		443: listener.Addr().String(),
	})}
	defer exitB.Close()
	generator := &reraceSdkGenerator{
		deviceSettings: settings,
		exits:          []*reraceSdkExit{exitA, exitB},
		clientExits:    map[connect.Id]*reraceSdkExit{},
		unsubs:         map[connect.Id]func(){},
	}
	settings.GeneratorFunc = func([]*connect.ProviderSpec) connect.MultiClientGenerator {
		return generator
	}

	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	// the provider-qualification sweep dials public health hosts through the
	// exits; keep every other shipped reliability default
	reliabilitySettings := reliabilitySettingsFromConnect(
		connect.ReliabilitySettingsFrom(connect.DefaultMultiClientSettings()),
	)
	reliabilitySettings.ProviderProbe = false
	device.SetReliabilitySettings(reliabilitySettings)
	upgradeMuxSettings := connect.DefaultUpgradeMuxSettings()
	upgradeMuxSettings.Dns = nil
	device.SetUpgradeMuxSettings(upgradeMuxSettings)
	bridge := newSyntheticTunnelBridge(t, device)
	defer bridge.Close()

	device.SetConnectLocation(&ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}})
	device.SetRouteLocal(false)

	windowDeadline := time.Now().Add(30 * time.Second)
	for {
		live := 0
		exits := device.GetExits()
		for i := 0; i < exits.Len(); i += 1 {
			if exit := exits.Get(i); !exit.Done && !exit.Warning && exit.WindowType == "quality" {
				live += 1
			}
		}
		if live == 2 {
			break
		}
		if time.Now().After(windowDeadline) {
			t.Fatalf("the window holds %d live exits, want 2", live)
		}
		time.Sleep(20 * time.Millisecond)
	}

	dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
	defer dialCancel()
	start := time.Now()
	conn, err := bridge.tun.DialContext(dialCtx, "tcp", net.JoinHostPort("203.0.113.44", strconv.Itoa(443)))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("the app's connect failed after %s: %v", elapsed, err)
	}
	defer conn.Close()
	t.Logf("connected through the re-race after %s", elapsed)
	if 3*time.Second <= elapsed {
		t.Errorf("the connect took %s, want within about one retransmit: before the 3s silence inference", elapsed)
	}

	// the connection is B's: bytes round-trip through B to the echo server
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 5)
	if _, err := io.ReadFull(conn, reply); err != nil || string(reply) != "hello" {
		t.Fatalf("echo = %q, %v", reply, err)
	}
	if got := exitA.router.Dials(443); got != 1 {
		t.Errorf("A dialed 443 %d times, want once", got)
	}
	if got := exitB.router.Dials(443); got != 1 {
		t.Errorf("B dialed 443 %d times, want once", got)
	}
	destinationExits := device.GetDestinationExits()
	carried := false
	for i := 0; i < destinationExits.Len(); i += 1 {
		destinationExit := destinationExits.Get(i)
		if destinationExit.DestinationIp != "203.0.113.44" {
			continue
		}
		clientId, err := connect.ParseId(destinationExit.ClientId.String())
		if err != nil {
			t.Fatal(err)
		}
		if exit := generator.exitOf(clientId); exit != exitB {
			t.Errorf("the flow rides exit %v, want B", exit)
		}
		carried = true
	}
	if !carried {
		t.Error("no live flow to the destination")
	}
	metrics := device.GetReliabilityMetrics()
	if metrics.DialFailuresIntercepted < 1 || metrics.FlowsReraced < 1 {
		t.Errorf("DialFailuresIntercepted=%d FlowsReraced=%d, want the signal and its re-race", metrics.DialFailuresIntercepted, metrics.FlowsReraced)
	}
	if err := bridge.Error(); err != nil {
		t.Errorf("tunnel bridge: %v", err)
	}
}
