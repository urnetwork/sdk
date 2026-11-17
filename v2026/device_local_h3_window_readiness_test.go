//go:build !ios

// Exercise the mobile carrier ledger together with real public-window admission.
// Only discovery/auth and the remote exchange are local fixtures. No public
// network, device, or production credential participates.
package sdk

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// Discovery is held by window class, not by a timer or scheduler-dependent
// sleep. Each class has enough distinct synthetic providers for its real
// configured minimum; this fixture never fabricates an Added event.
type mobileH3WindowDiscovery struct {
	gates       map[string]<-chan struct{}
	providers   map[string][]*connect.Client
	windowTypes map[connect.Id]connect.WindowType
}

func (self *mobileH3WindowDiscovery) FindProviders2(
	ctx context.Context,
	_ string,
	args *connect.FindProviders2Args,
) (*connect.FindProviders2Result, error) {
	gate, ok := self.gates[args.RankMode]
	if !ok {
		return nil, errors.New("unexpected synthetic window class")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-gate:
	}
	excluded := make(map[connect.Id]bool)
	for _, id := range args.ExcludeClientIds {
		excluded[id] = true
	}
	for _, path := range args.ExcludeDestinations {
		for _, id := range path {
			excluded[id] = true
		}
	}
	result := &connect.FindProviders2Result{}
	for _, provider := range self.providers[args.RankMode] {
		if !excluded[provider.ClientId()] {
			result.Providers = append(result.Providers, &connect.FindProvidersProvider{
				ClientId: provider.ClientId(),
			})
		}
	}
	return result, nil
}

// Preserve the real API generator's destination-aware, context-aware entry.
// The map only labels constructor observations; the generator, carrier, ping
// and window own their normal production admission and cancellation.
type mobileH3WindowGenerator struct {
	*h1OwnerGenerator
	stateLock         sync.Mutex
	clientWindowTypes map[connect.Id]connect.WindowType
	discovery         *mobileH3WindowDiscovery
}

func (self *mobileH3WindowGenerator) NewClientArgsForDestination(destination connect.MultiHopId) (*connect.MultiClientGeneratorClientArgs, error) {
	return self.NewClientArgsForDestinationContext(context.Background(), destination)
}

func (self *mobileH3WindowGenerator) NewClientArgsForDestinationContext(
	ctx context.Context,
	destination connect.MultiHopId,
) (*connect.MultiClientGeneratorClientArgs, error) {
	args, err := self.ApiMultiClientGenerator.NewClientArgsForDestinationContext(ctx, destination)
	if err == nil {
		self.stateLock.Lock()
		self.clientWindowTypes[args.ClientId] = self.discovery.windowTypes[destination.Tail()]
		self.stateLock.Unlock()
	}
	return args, err
}

// A legacy H3 peer retains the real QUIC/auth/framing/route boundary while
// using the existing H1 fixture only as its local exchange backend. Declining
// DATAGRAM negotiation is supported protocol behavior, not a memory-policy
// override: the client still constructs and charges its complete H3 graph.
func newMobileH3WindowExchange(t *testing.T, fixture *h1OwnerFixture) (int, <-chan error) {
	t.Helper()
	certPem, keyPem, err := generateSelfSignedCert()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(fixture.ctx)
	listener, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"mobile-window-readiness"},
	}, &quic.Config{})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	failures := make(chan error, 1)
	report := func(err error) {
		if err != nil && ctx.Err() == nil {
			select {
			case failures <- err:
			default:
			}
		}
	}
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := listener.Accept(ctx)
			if err != nil {
				report(err)
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.CloseWithError(0, "synthetic exchange complete")
				handlerDone, cancelDone := make(chan struct{}), make(chan struct{})
				go func() {
					defer close(cancelDone)
					select {
					case <-ctx.Done():
						conn.CloseWithError(0, "synthetic exchange owner closed")
					case <-handlerDone:
					}
				}()
				defer func() { close(handlerDone); <-cancelDone }()
				stream, err := conn.AcceptStream(ctx)
				if err != nil {
					if conn.Context().Err() == nil {
						report(err)
					}
					return
				}
				framer := connect.NewFramer(connect.DefaultFramerSettings(1024 * 1024))
				authBytes, err := framer.Read(stream)
				if err != nil {
					report(err)
					return
				}
				message, err := connect.DecodeFrame(authBytes)
				connect.MessagePoolReturn(authBytes)
				if err != nil {
					report(err)
					return
				}
				auth, ok := message.(*protocol.Auth)
				if !ok {
					report(fmt.Errorf("unexpected synthetic auth type %T", message))
					return
				}
				headers := http.Header{"Authorization": []string{"Bearer " + auth.ByJwt}}
				ws, _, err := websocket.DefaultDialer.DialContext(ctx,
					"ws"+strings.TrimPrefix(fixture.server.URL, "http")+"/h1", headers)
				if err != nil {
					report(err)
					return
				}
				defer ws.Close()
				response, accepted := connect.AcceptH3DatagramAuthOffer(auth, false, false, false)
				if accepted {
					report(errors.New("legacy synthetic peer negotiated datagrams"))
					return
				}
				responseBytes, err := connect.EncodeFrame(response, connect.DefaultProtocolVersion)
				if err != nil {
					report(err)
					return
				}
				err = framer.Write(stream, responseBytes)
				connect.MessagePoolReturn(responseBytes)
				if err != nil {
					report(err)
					return
				}
				returnDone := make(chan struct{})
				go func() {
					defer close(returnDone)
					defer conn.CloseWithError(0, "synthetic return complete")
					for {
						_, packet, readErr := ws.ReadMessage()
						if readErr != nil {
							return
						}
						if writeErr := framer.Write(stream, packet); writeErr != nil {
							return
						}
					}
				}()
				for {
					packet, readErr := framer.Read(stream)
					if readErr != nil {
						break
					}
					writeErr := ws.WriteMessage(websocket.BinaryMessage, packet)
					connect.MessagePoolReturn(packet)
					if writeErr != nil {
						break
					}
				}
				ws.Close()
				<-returnDone
			}()
		}
	}()
	t.Cleanup(func() {
		cancel()
		listener.Close()
		workers.Wait()
	})
	return listener.Addr().(*net.UDPAddr).Port, failures
}

// The actual DeviceLocal and destination constructors run on a native test
// host with the existing explicit mobile-platform seam. A fresh
// owner rules out retained prior device generations. The semantic failure
// requires a real Added provider, a real connected H3 route, an independently
// constructed blocked sibling, and both actual minima being unreachable with
// the remaining ledger. A watchdog is only a fixture-hang failure.
func testMobilePublicCarrierReadiness(
	t *testing.T,
	target ByteCount,
	mode connect.TransportMode,
	firstWindow connect.WindowType,
	fixedPeer bool,
) {
	t.Helper()
	previous := connect.MemoryBudget()
	connect.SetMemoryBudget(target)
	t.Cleanup(func() { connect.SetMemoryBudget(previous) })

	settings := connect.DefaultMultiClientSettings()
	applyMobileLowMemoryMultiClientSettingsForPlatform(settings, target, true)
	settings.Log = connect.NewNoopLogger()
	requiredQuality := settings.WindowSizes[connect.WindowTypeQuality].WindowSizeMin
	requiredSpeed := settings.WindowSizes[connect.WindowTypeSpeed].FixedWindowSize
	providerCount := requiredQuality + requiredSpeed
	if fixedPeer {
		providerCount = 1
	}
	fixture := newH1OwnerFixtureWithProviders(t, providerCount, 45*time.Second)
	port, peerFailures := newMobileH3WindowExchange(t, fixture)
	ctx, cancel := context.WithCancel(fixture.ctx)
	t.Cleanup(cancel)
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	strategySettings.ExtenderConfigs = nil
	strategySettings.DnsTlds = nil
	strategySettings.InternalDohDomains = nil
	strategy := connect.NewClientStrategy(ctx, strategySettings)
	t.Cleanup(strategy.Close)
	var generator *mobileH3WindowGenerator
	var generatorFactory func([]*connect.ProviderSpec) connect.MultiClientGenerator
	deviceSettings := DefaultLowMemoryDeviceLocalSettings()
	deviceSettings.MemoryTargetByteCount = target
	deviceSettings.AllowProvider = false
	deviceSettings.DisableLogging = true
	deviceSettings.GeneratorFunc = func(specs []*connect.ProviderSpec) connect.MultiClientGenerator {
		return generatorFactory(specs)
	}
	device, err := newDeviceLocalWithOverridesForPlatform(fixture.space,
		testingRefreshableJwtWithMarker(t, "synthetic-h3-window"),
		"mobile-window", "test", "0", NewId(), deviceSettings, connect.NewId(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		join, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := device.CloseAndWait(join); err != nil {
			t.Error(err)
		}
	})
	upgrade := connect.DefaultUpgradeMuxSettings()
	upgrade.Dns = nil
	device.SetUpgradeMuxSettings(upgrade)
	owner := device.platformTransportBudget
	if owner.Stats().TotalByteCount != target/4 || owner.Stats().UsedByteCount != 0 {
		t.Fatal("fixture did not start with the actual fresh mobile carrier owner")
	}
	qualityGate, speedGate := make(chan struct{}), make(chan struct{})
	if firstWindow == connect.WindowTypeQuality || fixedPeer {
		close(qualityGate)
	} else {
		close(speedGate)
	}
	discovery := &mobileH3WindowDiscovery{
		gates:       map[string]<-chan struct{}{"quality": qualityGate, "speed": speedGate},
		providers:   make(map[string][]*connect.Client),
		windowTypes: make(map[connect.Id]connect.WindowType),
	}
	for i, provider := range fixture.providers {
		window := connect.WindowTypeQuality
		if !fixedPeer && i >= requiredQuality {
			window = connect.WindowTypeSpeed
		}
		discovery.providers[window.RankMode()] = append(discovery.providers[window.RankMode()], provider)
		discovery.windowTypes[provider.ClientId()] = window
	}
	generator = &mobileH3WindowGenerator{
		h1OwnerGenerator:  &h1OwnerGenerator{providers: fixture.providers},
		clientWindowTypes: make(map[connect.Id]connect.WindowType),
		discovery:         discovery,
	}
	events := connect.NewMonitor()
	type carrierObservation struct {
		carrier *connect.PlatformTransport
		window  connect.WindowType
	}
	var stateLock sync.Mutex
	carriers := make([]carrierObservation, 0, providerCount)
	var claim connect.ByteCount
	apiSettings := connect.DefaultApiMultiClientGeneratorSettings()
	apiSettings.ProviderDiscovery = discovery
	apiSettings.PlatformTransportMode = mode
	_, preferences := toConnectTransportPolicy(DefaultTransportSettings(), false)
	apiSettings.PlatformTransportModePreferences = preferences
	apiSettings.PlatformTransportSettingsGenerator = func() *connect.PlatformTransportSettings {
		transport := newDeviceLocalPlatformTransportSettings(target, owner, nil,
			fmt.Sprintf("https://127.0.0.1:%d", port), "")
		applyMobileLowMemoryPlatformTransportSettingsForPlatform(transport, target, true)
		transport.V2H1Auth = true
		transport.H3Port = port
		transport.H3PacketConnFactory = func(context.Context) (net.PacketConn, error) {
			return net.ListenPacket("udp4", "127.0.0.1:0")
		}
		transport.QuicTlsConfig = &tls.Config{InsecureSkipVerify: true,
			NextProtos: []string{"mobile-window-readiness"}}
		transport.SendRouteObserver = func(_ connect.Transport, _ connect.Route, _ bool) {
			events.NotifyAll()
		}
		stateLock.Lock()
		if claim != 0 && claim != transport.H3BudgetByteCount {
			t.Error("one owner constructed inconsistent H3 claims")
		}
		claim = transport.H3BudgetByteCount
		stateLock.Unlock()
		return transport
	}
	apiSettings.PlatformTransportCreated = func(client *connect.Client, carrier *connect.PlatformTransport) {
		generator.stateLock.Lock()
		window, known := generator.clientWindowTypes[client.ClientId()]
		generator.stateLock.Unlock()
		if !known && fixedPeer {
			window, known = connect.WindowTypeQuality, true
		}
		if !known {
			t.Error("carrier bypassed the destination-aware constructor observation")
		}
		stateLock.Lock()
		carriers = append(carriers, carrierObservation{carrier: carrier, window: window})
		stateLock.Unlock()
		events.NotifyAll()
	}
	clientSettings := func() *connect.ClientSettings {
		client := newDeviceClientSettings(
			connect.DefaultClientSettingsWithBufferSize(deviceLocalSequenceBufferSize(target)),
			fixture.server.URL, strategy)
		applyMobileLowMemoryClientSettingsForPlatform(client, target, true)
		applyMobileH1PerformanceClientSettingsForPlatform(client, target, true, mode == connect.TransportModeH1)
		client.EncryptionSettings.Mode = connect.EncryptionModeOff
		client.Log = connect.NewNoopLogger()
		return client
	}
	generatorFactory = func(specs []*connect.ProviderSpec) connect.MultiClientGenerator {
		generator.ApiMultiClientGenerator = connect.NewApiMultiClientGenerator(ctx, specs,
			strategy, nil, fixture.server.URL, "synthetic-window-owner",
			fixture.space.platformUrl, "mobile-window", "test", "0", nil, clientSettings, apiSettings)
		return generator
	}
	t.Cleanup(func() {
		defer cancel()
		join, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := generator.CloseAndWait(join); err != nil {
			t.Error(err)
		}
		stats := owner.Stats()
		if stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 ||
			stats.ReservedByteCount != stats.ReleasedByteCount {
			t.Errorf("joined current window retained carrier claims: %+v", stats)
		}
	})
	device.SetTransportSettings(&TransportSettings{Mode: TransportMode(mode)})
	location := &ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}}
	if fixedPeer {
		location.ConnectLocationId = &ConnectLocationId{ClientId: newId(fixture.providers[0].ClientId())}
	}
	device.SetConnectLocation(location)
	device.stateLock.Lock()
	multi, ok := device.remoteUserNatClient.(*connect.RemoteUserNatMultiClient)
	device.stateLock.Unlock()
	if !ok {
		t.Fatal("actual destination did not construct a multi-client")
	}
	t.Cleanup(func() {
		multi.Close()
		join, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := multi.CloseAndWait(join); err != nil {
			t.Error(err)
		}
	})
	unsubscribe := multi.Monitor().AddMonitorEventCallback(func(
		_ *connect.WindowExpandEvent, _ map[connect.Id]*connect.ProviderEvent, _ bool,
	) {
		events.NotifyAll()
	})
	t.Cleanup(unsubscribe)
	siblingReleased := fixedPeer
	for {
		changed := events.NotifyChannel()
		capacityChanged := owner.CapacityNotify()
		qualityCount, speedCount := 0, 0
		for _, exit := range multi.Exits() {
			if exit.Done || exit.Warning || exit.Quarantined {
				continue
			}
			if exit.WindowType == connect.WindowTypeQuality {
				qualityCount++
			} else if exit.WindowType == connect.WindowTypeSpeed {
				speedCount++
			}
		}
		if !siblingReleased && (firstWindow == connect.WindowTypeQuality && qualityCount > 0 ||
			firstWindow == connect.WindowTypeSpeed && speedCount > 0) {
			siblingReleased = true
			if firstWindow == connect.WindowTypeQuality {
				close(speedGate)
			} else {
				close(qualityGate)
			}
		}
		window := multi.Monitor().WindowExpandEvent()
		stateLock.Lock()
		observed := append([]carrierObservation(nil), carriers...)
		currentClaim := claim
		stateLock.Unlock()
		connected, blocked, siblingConstructed := 0, 0, false
		for _, observation := range observed {
			if observation.carrier.IsConnected() {
				connected++
			}
			if observation.carrier.IsWaitingForBudget() {
				blocked++
			}
			if observation.window != firstWindow {
				siblingConstructed = true
			}
		}
		stats := owner.Stats()
		if stats.UsedByteCount > stats.TotalByteCount || stats.UsedTransportCount > stats.MaxTransportCount {
			t.Fatalf("constructor escaped the current owner: %+v", stats)
		}
		if window.MinSatisfied && !window.Failed && connected > 0 && qualityCount+speedCount > 0 {
			t.Logf("MOBILE_CARRIER_READY target=%d mode=%s first=%d quality=%d/%d speed=%d/%d connected=%d blocked=%d used=%d cap=%d",
				target, mode, firstWindow, qualityCount, requiredQuality, speedCount, requiredSpeed,
				connected, blocked, stats.UsedByteCount, stats.TotalByteCount)
			return
		}
		if mode == connect.TransportModeH3 && !fixedPeer && !window.MinSatisfied && siblingConstructed &&
			window.TargetSize == requiredQuality+requiredSpeed &&
			connected > 0 && blocked > 0 && qualityCount+speedCount > 0 && currentClaim > 0 {
			remainingCarriers := int((stats.TotalByteCount - stats.UsedByteCount) / currentClaim)
			if qualityCount+remainingCarriers < requiredQuality && speedCount+remainingCarriers < requiredSpeed {
				t.Fatalf("MOBILE_H3_READINESS_UNREACHABLE target=%d first=%d quality=%d/%d speed=%d/%d connected=%d blocked=%d used=%d cap=%d claim=%d remaining=%d min_satisfied=%t",
					target, firstWindow, qualityCount, requiredQuality, speedCount, requiredSpeed,
					connected, blocked, stats.UsedByteCount, stats.TotalByteCount, currentClaim,
					remainingCarriers, window.MinSatisfied)
			}
		}
		select {
		case <-changed:
		case <-capacityChanged:
		case err := <-peerFailures:
			t.Fatalf("synthetic H3 exchange failed before readiness: %v", err)
		case <-ctx.Done():
			t.Fatalf("fixture watchdog, not a semantic capacity witness: %v", ctx.Err())
		}
	}
}

// The quality-first ordering must not strand public H3 behind an unreachable
// quality minimum while its independently constructed speed sibling waits.
func TestMobileH3PublicReadiness32QualityFirst(t *testing.T) {
	testMobilePublicCarrierReadiness(t, 32*1024*1024, connect.TransportModeH3, connect.WindowTypeQuality, false)
}

func TestMobileH3PublicReadiness64QualityFirst(t *testing.T) {
	testMobilePublicCarrierReadiness(t, 64*1024*1024, connect.TransportModeH3, connect.WindowTypeQuality, false)
}

// Reversing the admission order distinguishes the 32-MiB race from the
// 64-MiB minimum of two; neither control depends on the scheduler winning.
func TestMobileH3PublicReadiness32SpeedFirst(t *testing.T) {
	testMobilePublicCarrierReadiness(t, 32*1024*1024, connect.TransportModeH3, connect.WindowTypeSpeed, false)
}

func TestMobileH3PublicReadiness64SpeedFirst(t *testing.T) {
	testMobilePublicCarrierReadiness(t, 64*1024*1024, connect.TransportModeH3, connect.WindowTypeSpeed, false)
}

// Lower-cost H1 and optional Auto-H3 must preserve actual public admission.
func TestMobileH1PublicReadiness32(t *testing.T) {
	testMobilePublicCarrierReadiness(t, 32*1024*1024, connect.TransportModeH1, connect.WindowTypeQuality, false)
}

func TestMobileH1PublicReadiness64(t *testing.T) {
	testMobilePublicCarrierReadiness(t, 64*1024*1024, connect.TransportModeH1, connect.WindowTypeQuality, false)
}

func TestMobileAutoPublicReadiness32(t *testing.T) {
	testMobilePublicCarrierReadiness(t, 32*1024*1024, connect.TransportModeAuto, connect.WindowTypeQuality, false)
}

func TestMobileAutoPublicReadiness64(t *testing.T) {
	testMobilePublicCarrierReadiness(t, 64*1024*1024, connect.TransportModeAuto, connect.WindowTypeQuality, false)
}

// A selected single peer is a different production minimum, not permission
// to relabel public discovery as fixed or to claim native P2P coverage.
func TestMobileH3FixedPeerReadiness32(t *testing.T) {
	testMobilePublicCarrierReadiness(t, 32*1024*1024, connect.TransportModeH3, connect.WindowTypeQuality, true)
}

func TestMobileH3FixedPeerReadiness64(t *testing.T) {
	testMobilePublicCarrierReadiness(t, 64*1024*1024, connect.TransportModeH3, connect.WindowTypeQuality, true)
}
