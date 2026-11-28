package sdk

// A device whose space egresses through a proxy, as an sn miner's proxy slot's
// does (connect#227, owner 2026-11-18, "route through the proxy"): its public
// platform transports, its exit's tcp and udp, its p2p streams and the provider
// extender role. Each test puts a recording socks proxy where the space's
// traffic should go and a canary where this host's own traffic would land, and
// keeps a space with no proxy as the control where the control proves
// something.
//
// Addresses are loopback and RFC 5737 documentation addresses.

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// A socks5 server on loopback that reads one connect request per connection,
// records its target as "<host>:<port>" and refuses it.
type testProxiedSpaceSocks struct {
	listener net.Listener
	targets  chan string
}

func newTestProxiedSpaceSocks(t *testing.T) *testProxiedSpaceSocks {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	socks := &testProxiedSpaceSocks{
		listener: listener,
		targets:  make(chan string, 64),
	}
	serve := func(conn net.Conn) {
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		greeting := make([]byte, 2)
		if _, err := io.ReadFull(conn, greeting); err != nil {
			return
		}
		if _, err := io.ReadFull(conn, make([]byte, greeting[1])); err != nil {
			return
		}
		// no authentication
		if _, err := conn.Write([]byte{5, 0}); err != nil {
			return
		}
		request := make([]byte, 4)
		if _, err := io.ReadFull(conn, request); err != nil {
			return
		}
		var host string
		switch request[3] {
		case 1:
			ip := make([]byte, 4)
			if _, err := io.ReadFull(conn, ip); err != nil {
				return
			}
			host = netip.AddrFrom4([4]byte(ip)).String()
		case 4:
			ip := make([]byte, 16)
			if _, err := io.ReadFull(conn, ip); err != nil {
				return
			}
			host = netip.AddrFrom16([16]byte(ip)).String()
		case 3:
			nameLength := make([]byte, 1)
			if _, err := io.ReadFull(conn, nameLength); err != nil {
				return
			}
			name := make([]byte, nameLength[0])
			if _, err := io.ReadFull(conn, name); err != nil {
				return
			}
			host = string(name)
		default:
			return
		}
		port := make([]byte, 2)
		if _, err := io.ReadFull(conn, port); err != nil {
			return
		}
		select {
		case socks.targets <- fmt.Sprintf("%s:%d", host, binary.BigEndian.Uint16(port)):
		default:
		}
		// connection refused
		conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
	}
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				serve(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		wg.Wait()
	})
	return socks
}

func (self *testProxiedSpaceSocks) proxySettings() *connect.ProxySettings {
	return &connect.ProxySettings{
		Network: "tcp",
		Address: self.listener.Addr().String(),
	}
}

// Waits until the proxy has been asked for `target`, and fails at once when
// `leaked` names a canary this host reached on its own.
func (self *testProxiedSpaceSocks) waitTarget(t *testing.T, ctx context.Context, leaked *testProxiedSpaceTcpCanary, target string) {
	t.Helper()
	seen := []string{}
	for !slices.Contains(seen, target) {
		select {
		case next := <-self.targets:
			seen = append(seen, next)
		case <-leaked.accepted:
			t.Fatalf("%s was reached from this host, outside the proxy", leaked.listener.Addr())
		case <-ctx.Done():
			t.Fatalf("the proxy was asked for %v, expected %s among them", seen, target)
		}
	}
}

// A tcp listener on loopback that counts the connections that reach it and
// signals each on `accepted`.
type testProxiedSpaceTcpCanary struct {
	listener      net.Listener
	acceptedCount atomic.Int64
	accepted      chan struct{}
}

func newTestProxiedSpaceTcpCanary(t *testing.T) *testProxiedSpaceTcpCanary {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	canary := &testProxiedSpaceTcpCanary{
		listener: listener,
		accepted: make(chan struct{}, 16),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			canary.acceptedCount.Add(1)
			select {
			case canary.accepted <- struct{}{}:
			default:
			}
			conn.Close()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-done
	})
	return canary
}

// The client strategy settings of a space whose egress is `proxySettings`.
func testProxiedSpaceStrategySettings(proxySettings *connect.ProxySettings) *connect.ClientStrategySettings {
	settings := connect.DefaultClientStrategySettings()
	settings.ProxySettings = proxySettings
	return settings
}

// A public provider in a proxied space reaches the platform through the proxy,
// so the platform sees the proxy's address, not this host's. Before, the
// standby of a public provider took a direct strategy that dropped the proxy
// (J4) and dialed the platform from this host.
func TestDeviceLocalProviderPublicStandbyDialsThroughTheSpaceProxy(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	socks := newTestProxiedSpaceSocks(t)
	platform := newTestProxiedSpaceTcpCanary(t)
	client, sharedStrategy := newTestProviderClient(t)
	provider := &deviceLocalProvider{
		ctx:                       client.Ctx(),
		client:                    client,
		clientStrategy:            sharedStrategy,
		clientStrategySettings:    testProxiedSpaceStrategySettings(socks.proxySettings()),
		platformUrl:               "ws://" + platform.listener.Addr().String(),
		platformTransportSettings: connect.DefaultPlatformTransportSettings(),
		targetMode:                connect.TransportModeH1,
		modePreferences:           connect.DefaultTransportModePreferences(),
		transportPolicyVersion:    1,
		migrateConnectTimeout:     connect.DefaultApiMultiClientGeneratorSettings().MigrateConnectTimeout,
		migrateMaxScheduleDelay:   connect.DefaultApiMultiClientGeneratorSettings().MigrateMaxScheduleDelay,
		provideMode:               ProvideModePublic,
		auth: &connect.ClientAuth{
			ByJwt:      "test",
			InstanceId: connect.NewId(),
			AppVersion: "0.0.0",
		},
	}
	t.Cleanup(func() {
		provider.stateLock.Lock()
		egressStandbyStrategy := provider.egressStandbyStrategy
		provider.stateLock.Unlock()
		if egressStandbyStrategy != nil {
			egressStandbyStrategy.Close()
		}
	})
	transport := provider.newProviderPlatformTransport(
		provider.auth,
		provider.targetMode,
		provider.platformTransportSettings,
	)
	defer waitTransportDone(t, transport)

	socks.waitTarget(t, ctx, platform, platform.listener.Addr().String())
	if count := platform.acceptedCount.Load(); count != 0 {
		t.Fatalf("the standby reached the platform from this host %d times", count)
	}
}

// A device whose exit egress the test can drive: a provider client and the
// exit nat the device builds for it, in `networkSpace`.
func newTestProxiedSpaceExitDevice(t *testing.T, networkSpace *NetworkSpace) *DeviceLocal {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	client := connect.NewClient(
		ctx,
		connect.NewId(),
		connect.NewNoContractClientOob(),
		connect.DefaultClientSettings(),
	)
	settings := DefaultDeviceLocalSettings()
	settings.MemoryTargetByteCount = 0
	settings.ProvideExtenderEnabled = true
	device := &DeviceLocal{
		ctx:                                ctx,
		cancel:                             cancel,
		settings:                           settings,
		networkSpace:                       networkSpace,
		clientId:                           connect.NewId(),
		provider:                           &deviceLocalProvider{client: client, networkSpace: networkSpace},
		provideMode:                        ProvideModePublic,
		lifecycleDone:                      make(chan struct{}),
		providerPacketStatsChangeListeners: connect.NewCallbackList[PacketStatsChangeListener](),
	}
	device.stateLock.Lock()
	err := device.ensureRemoteUserNatProviderWithLock()
	device.stateLock.Unlock()
	if err != nil || device.remoteUserNatProviderLocalUserNat == nil {
		t.Fatalf("the device built no exit: %v", err)
	}
	t.Cleanup(func() {
		device.stateLock.Lock()
		device.closed = true
		device.provideMode = ProvideModeNone
		device.closeRemoteUserNatProviderWithLock()
		device.stateLock.Unlock()
		device.lifecycleWorkers.Wait()
		cancel()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := client.CloseAndWait(closeCtx); err != nil {
			t.Errorf("close provider client: %v", err)
		}
	})
	return device
}

// A provider's exit in a proxied space leaves through the proxy: its tcp
// reaches the destination through the proxy, and its udp, which the proxy
// cannot carry, is refused at once and answered, never sent from this host.
// Before, the exit nat took no proxy, since sn passes no
// ProviderDialContextSettings, and sent both from this host.
func TestDeviceLocalProxiedExitLeavesThroughTheSpaceProxy(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	socks := newTestProxiedSpaceSocks(t)
	tcpDestination := newTestProxiedSpaceTcpCanary(t)
	udpDestination, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udpDestination.Close()
	// whatever reaches the udp destination left this host
	leaked := make(chan string, 1)
	go func() {
		buffer := make([]byte, 64)
		n, _, err := udpDestination.ReadFrom(buffer)
		if err == nil {
			leaked <- string(buffer[:n])
		}
	}()

	networkSpace := &NetworkSpace{clientStrategySettings: testProxiedSpaceStrategySettings(socks.proxySettings())}
	device := newTestProxiedSpaceExitDevice(t, networkSpace)
	localUserNat := device.remoteUserNatProviderLocalUserNat

	// the refused udp flow's answer back toward the source
	unreachables := make(chan struct{}, 4)
	unsub := localUserNat.AddReceivePacketCallback(func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		// an icmp packet (protocol 1) on the udp flow
		if ipPath.Protocol == connect.IpProtocolUdp && 20 <= len(packet) && packet[0]>>4 == 4 && packet[9] == 1 {
			select {
			case unreachables <- struct{}{}:
			default:
			}
		}
	})
	defer unsub()

	source := connect.SourceId(connect.NewId())
	udpPort := udpDestination.LocalAddr().(*net.UDPAddr).Port
	datagram := craftIpv4Packet(connect.IpProtocolUdp, net.ParseIP("192.0.2.42"), 40000, net.ParseIP("127.0.0.1"), udpPort, false, []byte("exit"))
	// The loopback canary is eligible only for the authenticated Network relationship.
	if !localUserNat.SendPacket(source, protocol.ProvideMode_Network, datagram, -1) {
		t.Fatal("the datagram was not queued")
	}
	select {
	case <-unreachables:
	case payload := <-leaked:
		t.Fatalf("the exit's datagram %q left this host outside the proxy", payload)
	case <-ctx.Done():
		t.Fatal("the exit's refused udp flow was never answered")
	}
	select {
	case payload := <-leaked:
		t.Fatalf("the exit's datagram %q left this host outside the proxy", payload)
	default:
	}

	tcpPort := tcpDestination.listener.Addr().(*net.TCPAddr).Port
	for _, tc := range []struct {
		address string
		mode    protocol.ProvideMode
	}{
		{address: "203.0.113.47", mode: protocol.ProvideMode_Public},
		{address: "127.0.0.1", mode: protocol.ProvideMode_Network},
	} {
		syn := craftIpv4Packet(connect.IpProtocolTcp, net.ParseIP("192.0.2.42"), 40001, net.ParseIP(tc.address), tcpPort, true, nil)
		if !localUserNat.SendPacket(source, tc.mode, syn, -1) {
			t.Fatal("the syn was not queued")
		}
		socks.waitTarget(t, ctx, tcpDestination, fmt.Sprintf("%s:%d", tc.address, tcpPort))
	}
	if count := tcpDestination.acceptedCount.Load(); count != 0 {
		t.Fatalf("the exit's tcp reached its destination from this host %d times", count)
	}
}

// The control: an exit in a space with no proxy leaves from this host, as
// every app's does.
func TestDeviceLocalUnproxiedExitLeavesFromThisHost(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	tcpDestination := newTestProxiedSpaceTcpCanary(t)
	networkSpace := &NetworkSpace{clientStrategySettings: connect.DefaultClientStrategySettings()}
	device := newTestProxiedSpaceExitDevice(t, networkSpace)
	localUserNat := device.remoteUserNatProviderLocalUserNat

	tcpPort := tcpDestination.listener.Addr().(*net.TCPAddr).Port
	source := connect.SourceId(connect.NewId())
	refused := make(chan struct{}, 1)
	unsub := localUserNat.AddReceivePacketCallback(func(path connect.TransferPath, _ protocol.ProvideMode, _ *connect.IpPath, packet []byte) {
		// The NAT answers a policy refusal with an IPv4 destination unreachable.
		if path.SourceId == source.SourceId && len(packet) > 20 && packet[0] == 0x45 && packet[9] == 1 && packet[20] == 3 {
			select {
			case refused <- struct{}{}:
			default:
			}
		}
	})
	defer unsub()
	send := func(mode protocol.ProvideMode, port int) {
		syn := craftIpv4Packet(connect.IpProtocolTcp, net.ParseIP("192.0.2.42"), port, net.ParseIP("127.0.0.1"), tcpPort, true, nil)
		if !localUserNat.SendPacket(source, mode, syn, -1) {
			t.Fatal("the syn was not queued")
		}
	}
	send(protocol.ProvideMode_Public, 40001)
	select {
	case <-refused:
	case <-tcpDestination.accepted:
		t.Fatal("a Public exit reached the host listener")
	case <-ctx.Done():
		t.Fatal("the Public exit produced no refusal")
	}
	send(protocol.ProvideMode_Network, 40002)
	select {
	case <-tcpDestination.accepted:
	case <-ctx.Done():
		t.Fatal("the authorized Network exit never reached its local destination")
	}
}

// Every client of a proxied space opens no p2p stream and keeps the platform's
// observed addresses, its proxy's, out of the process's host guard, the
// provider's and the windows' alike, since their settings all pass through
// here; a client of a space with no proxy keeps p2p and applies them. The
// caller's settings are not changed.
func TestDeviceClientSettingsOfAProxiedSpaceOpenNoPeerConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	proxiedStrategy := connect.NewClientStrategy(ctx, testProxiedSpaceStrategySettings(&connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}))
	defer proxiedStrategy.Close()
	plainStrategy := connect.NewClientStrategy(ctx, connect.DefaultClientStrategySettings())
	defer plainStrategy.Close()

	callerSettings := connect.DefaultClientSettings()
	if settings := newDeviceClientSettings(callerSettings, "https://api.space.example", proxiedStrategy); !settings.WebRtcSettings.DisablePeerConnections {
		t.Fatal("a client of a proxied space keeps peer connections")
	}
	if callerSettings.WebRtcSettings.DisablePeerConnections {
		t.Fatal("the caller's settings were changed")
	}
	if settings := newDeviceClientSettings(nil, "https://api.space.example", proxiedStrategy); !settings.WebRtcSettings.DisablePeerConnections {
		t.Fatal("a client of a proxied space built from the defaults keeps peer connections")
	}
	if settings := newDeviceClientSettings(callerSettings, "https://api.space.example", plainStrategy); settings.WebRtcSettings.DisablePeerConnections {
		t.Fatal("a client of a space with no proxy lost peer connections")
	}

	if settings := newDeviceClientSettings(callerSettings, "https://api.space.example", proxiedStrategy); !settings.SkipObservedAddresses {
		t.Fatal("a client of a proxied space applies its proxy's observed addresses to the host guard")
	}
	if callerSettings.SkipObservedAddresses {
		t.Fatal("the caller's settings were changed")
	}
	if settings := newDeviceClientSettings(callerSettings, "https://api.space.example", plainStrategy); settings.SkipObservedAddresses {
		t.Fatal("a client of a space with no proxy skips the platform's observed addresses")
	}
}

// A device in a proxied space never asks for the provider extender role, even
// with the embedder's switch on and providing; the same device in a space with
// no proxy does. Before, the role ran from this host for whichever proxy slot
// the miner gave the switch.
func TestDeviceLocalProxiedSpaceNeverAsksForTheExtenderRole(t *testing.T) {
	proxied := newTestProxiedSpaceExitDevice(t, &NetworkSpace{clientStrategySettings: testProxiedSpaceStrategySettings(&connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"})})
	if !proxied.GetProvideEnabled() || !proxied.settings.ProvideExtenderEnabled {
		t.Fatal("the fixture does not provide with the switch on, so it proves nothing")
	}
	if proxied.extenderProvideProviding() {
		t.Fatal("a device in a proxied space would run the extender role")
	}
	plain := newTestProxiedSpaceExitDevice(t, &NetworkSpace{clientStrategySettings: connect.DefaultClientStrategySettings()})
	if !plain.extenderProvideProviding() {
		t.Fatal("a device in a space with no proxy would not run the extender role")
	}
}

// The provider holds the role off in a proxied space whatever it is asked: it
// never builds the role's settings, so nothing binds and no start error stands.
// Before, it built them and tried to start the role.
func TestDeviceLocalProviderInAProxiedSpaceStartsNoExtenderRole(t *testing.T) {
	testEnableExtenderProvideRole(t)
	client, _ := newTestProviderClient(t)
	networkSpace := &NetworkSpace{clientStrategySettings: testProxiedSpaceStrategySettings(&connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"})}
	settingsBuiltCount := atomic.Int64{}
	provider := &deviceLocalProvider{
		ctx:          client.Ctx(),
		client:       client,
		networkSpace: networkSpace,
		extenderSettingsConfigure: func(settings *deviceLocalExtenderSettings) {
			settingsBuiltCount.Add(1)
		},
	}
	provider.setExtenderEnabled(true)
	provider.stateLock.Lock()
	extender := provider.extender
	extenderStartError := provider.extenderStartError
	provider.stateLock.Unlock()
	if extender != nil {
		extender.Close()
		t.Fatal("the provider ran the extender role in a proxied space")
	}
	if count := settingsBuiltCount.Load(); count != 0 {
		t.Fatalf("the provider built the role's settings %d times", count)
	}
	if extenderStartError != "" {
		t.Fatalf("the role was asked to start and failed: %s", extenderStartError)
	}
}
