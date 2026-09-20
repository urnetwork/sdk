//go:build !ios

package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// Only discovery/auth and the remote packet origin are local fixtures. Window
// construction, H1 websocket, Transfer, SDK routing and generator retirement
// are production code. No control server or public address is contacted.
type h1OwnerGenerator struct {
	*connect.ApiMultiClientGenerator
	providers []*connect.Client
	mu        sync.Mutex
	clients   []*connect.Client
	carriers  []*connect.PlatformTransport
}

func (g *h1OwnerGenerator) FixedDestinationSize() (int, bool) {
	if len(g.providers) == 1 {
		return 1, true
	}
	// The multi-exit fixture exercises the real Auto quality/speed topology.
	// A five-destination fixed window would collide with the mobile per-window
	// quality ceiling rather than forming the production four-plus-one shape.
	return 0, false
}

func (g *h1OwnerGenerator) NewClient(ctx context.Context, args *connect.MultiClientGeneratorClientArgs, settings *connect.ClientSettings) (*connect.Client, error) {
	client, err := g.ApiMultiClientGenerator.NewClient(ctx, args, settings)
	if err == nil {
		for _, provider := range g.providers {
			client.ContractManager().AddNoContractPeer(provider.ClientId())
		}
		g.mu.Lock()
		g.clients = append(g.clients, client)
		g.mu.Unlock()
	}
	return client, err
}

type h1OwnerFixture struct {
	t         *testing.T
	ctx       context.Context
	space     *NetworkSpace
	providers []*connect.Client
	server    *httptest.Server
	wsWorkers sync.WaitGroup
	mu        sync.Mutex
	conns     map[*websocket.Conn]bool
	wireBytes atomic.Int64
	apiCalls  map[string]int
	// Opt-in fault injection for the topology experiment. Nil has no effect
	// on normal lifecycle/allocator fixtures; no production deadline changes.
	blackhole atomic.Pointer[connect.Id]
	dropped   atomic.Int64
}

// The local provider handoff terminates the same reliable H1 stream as the
// WebSocket. An untyped gateway would silently select lossy Pack admission.
type h1OwnerSendTransport struct{ connect.Transport }

func (*h1OwnerSendTransport) TransportType() connect.TransportType { return connect.TransportTypeH1 }

func h1OwnerRelayTransports(clientId connect.Id) (connect.Transport, connect.Transport, connect.TransferCarrierProperties) {
	return &h1OwnerSendTransport{connect.NewSendClientTransport(connect.DestinationId(clientId))},
		connect.NewReceiveGatewayTransportWithType(connect.TransportTypeH1),
		connect.TransferCarrierProperties{ReceiveReliability: connect.CarrierReliabilityReliable}
}

func newH1OwnerFixture(t *testing.T) *h1OwnerFixture {
	return newH1OwnerFixtureWithProviders(t, 1, 30*time.Second)
}

func newH1OwnerFixtureWithProviders(t *testing.T, providerCount int, timeout time.Duration) *h1OwnerFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	f := &h1OwnerFixture{t: t, ctx: ctx, conns: map[*websocket.Conn]bool{}, apiCalls: map[string]int{}}
	for range providerCount {
		provider, _ := startEchoProviderClient(t, ctx)
		f.providers = append(f.providers, provider)
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.apiCalls[r.URL.Path]++
		f.mu.Unlock()
		switch r.URL.Path {
		case "/hello", "/network/remove-client":
			_, _ = io.WriteString(w, `{}`)
		case "/network/auth-client":
			jwt := testingJwt(map[string]any{"client_id": connect.NewId().String(), "network_id": "00000000-0000-0000-0000-000000000004"})
			_ = json.NewEncoder(w).Encode(map[string]string{"by_client_jwt": jwt})
		case "/network/find-providers2":
			var args connect.FindProviders2Args
			_ = json.NewDecoder(r.Body).Decode(&args)
			excluded := map[connect.Id]bool{}
			for _, id := range args.ExcludeClientIds {
				excluded[id] = true
			}
			for _, ids := range args.ExcludeDestinations {
				for _, id := range ids {
					excluded[id] = true
				}
			}
			result := &connect.FindProviders2Result{}
			for _, provider := range f.providers {
				blocked := f.blackhole.Load()
				if !excluded[provider.ClientId()] && (blocked == nil || *blocked != provider.ClientId()) {
					result.Providers = append(result.Providers, &connect.FindProvidersProvider{ClientId: provider.ClientId()})
				}
			}
			_ = json.NewEncoder(w).Encode(result)
		case "/connect/control":
			_, _ = io.WriteString(w, `{"pack":"","error":null}`)
		case "/h1":
			jwt, err := connect.ParseByJwtUnverified(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			if err != nil {
				http.Error(w, "invalid fixture auth", http.StatusBadRequest)
				return
			}
			ws, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			f.wsWorkers.Add(1)
			defer f.wsWorkers.Done()
			f.mu.Lock()
			f.conns[ws] = true
			f.mu.Unlock()
			defer func() { f.mu.Lock(); delete(f.conns, ws); f.mu.Unlock() }()
			f.relay(ws, jwt.ClientId)
		default:
			http.NotFound(w, r)
		}
	}))
	f.space = newNetworkSpace(ctx, *NewNetworkSpaceKey("h1-owner.test", "test"), NetworkSpaceValues{
		ApiUrl: f.server.URL, PlatformUrl: "ws" + strings.TrimPrefix(f.server.URL, "http") + "/h1",
		NetExposeServerIps: true, NetExposeServerHostNames: true,
	}, t.TempDir())
	f.space.GetApi().tokenManager.Close()
	testingAwaitAuthBoundary(t, f.space.GetApi().tokenManager.done)
	t.Cleanup(func() {
		cancel()
		f.mu.Lock()
		for ws := range f.conns {
			_ = ws.Close()
		}
		f.mu.Unlock()
		f.wsWorkers.Wait()
		join, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		for _, provider := range f.providers {
			if err := provider.CloseAndWait(join); err != nil {
				t.Error(err)
			}
		}
		f.space.close()
		f.server.Close()
	})
	return f
}

func (f *h1OwnerFixture) relay(ws *websocket.Conn, clientId connect.Id) {
	ctx, cancel := context.WithCancel(f.ctx)
	toProviders, fromProvider := map[connect.Id]chan []byte{}, make(chan []byte)
	send, receive, properties := h1OwnerRelayTransports(clientId)
	for _, provider := range f.providers {
		toProvider := make(chan []byte)
		toProviders[provider.ClientId()] = toProvider
		provider.ContractManager().AddNoContractPeer(clientId)
		provider.RouteManager().UpdateTransportWithProperties(send, []connect.Route{fromProvider}, properties)
		provider.RouteManager().UpdateTransportWithProperties(receive, []connect.Route{toProvider}, properties)
	}
	var workers sync.WaitGroup
	workers.Go(func() {
		<-ctx.Done()
		_ = ws.Close()
	})
	workers.Go(func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case packet := <-fromProvider:
				if blocked := f.blackhole.Load(); blocked != nil {
					path, err := connect.FilteredTransferPath(packet)
					if err == nil && path.SourceId == *blocked {
						f.dropped.Add(1)
						connect.MessagePoolReturn(packet)
						continue
					}
				}
				err := ws.WriteMessage(websocket.BinaryMessage, packet)
				connect.MessagePoolReturn(packet)
				if err != nil {
					return
				}
			}
		}
	})
	defer func() {
		cancel()
		for _, provider := range f.providers {
			provider.RouteManager().RemoveTransport(send)
			provider.RouteManager().RemoveTransport(receive)
		}
		workers.Wait()
	}()
	for {
		_, packet, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if len(packet) <= 16 { // carrier keepalive, not a Transfer frame
			continue
		}
		f.wireBytes.Add(int64(len(packet)))
		path, err := connect.FilteredTransferPath(packet)
		if blocked := f.blackhole.Load(); err == nil && blocked != nil && path.DestinationId == *blocked {
			f.dropped.Add(1)
			continue
		}
		toProvider := toProviders[path.DestinationId]
		if err == nil && path.DestinationId == connect.ControlId && !path.IsStream() {
			// Preserve the original fixture's local control sink. These are
			// route/control frames, not packets to a public network endpoint.
			toProvider = toProviders[f.providers[0].ClientId()]
		}
		if err != nil || toProvider == nil {
			f.t.Errorf("invalid local H1 destination: bytes=%d parse=%v control=%t stream=%t", len(packet), err, path.IsControlDestination(), path.IsStream())
			return
		}
		owned := connect.MessagePoolCopy(packet)
		select {
		case <-ctx.Done():
			connect.MessagePoolReturn(owned)
			return
		case toProvider <- owned:
		}
	}
}

func (f *h1OwnerFixture) connectDevice(t *testing.T) (*DeviceLocal, *h1OwnerGenerator, *connect.RemoteUserNatMultiClient) {
	return f.connectDeviceWithTarget(t, 24*1024*1024)
}

func (f *h1OwnerFixture) connectDeviceWithTarget(t *testing.T, memoryTarget ByteCount) (*DeviceLocal, *h1OwnerGenerator, *connect.RemoteUserNatMultiClient) {
	t.Helper()
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider, settings.DisableLogging = false, true
	settings.MemoryTargetByteCount = memoryTarget
	var device *DeviceLocal
	var generator *h1OwnerGenerator
	settings.GeneratorFunc = func(specs []*connect.ProviderSpec) connect.MultiClientGenerator {
		createdGenerator := &h1OwnerGenerator{providers: f.providers}
		apiSettings := connect.DefaultApiMultiClientGeneratorSettings()
		apiSettings.PlatformTransportMode = connect.TransportModeH1
		apiSettings.PlatformTransportCreated = func(_ *connect.Client, transport *connect.PlatformTransport) {
			createdGenerator.mu.Lock()
			createdGenerator.carriers = append(createdGenerator.carriers, transport)
			createdGenerator.mu.Unlock()
		}
		apiSettings.PlatformTransportSettingsGenerator = func() *connect.PlatformTransportSettings {
			transport := newDeviceLocalPlatformTransportSettings(settings.MemoryTargetByteCount, device.platformTransportBudget, nil, "", "")
			applyMobileLowMemoryPlatformTransportSettings(transport, settings.MemoryTargetByteCount)
			transport.V2H1Auth = true
			return transport
		}
		clientSettings := func() *connect.ClientSettings {
			client := newDeviceClientSettings(connect.DefaultClientSettingsWithBufferSize(device.settings.SequenceBufferSize), f.server.URL, device.clientStrategy)
			client.Log = connect.NewNoopLogger()
			client.SendBufferSettings.ResendQueueBudget = device.settings.SendBufferSettings.ResendQueueBudget
			client.ReceiveBufferSettings.ReceiveQueueBudget = device.settings.ReceiveBufferSettings.ReceiveQueueBudget
			client.ReceiveBufferSettings.PackQueueBudget = device.settings.ReceiveBufferSettings.PackQueueBudget
			applyMobileLowMemoryClientSettings(client, settings.MemoryTargetByteCount)
			applyMobileH1PerformanceClientSettings(client, settings.MemoryTargetByteCount, true)
			client.EncryptionSettings.Mode = connect.EncryptionModeOff
			return client
		}
		api := connect.NewApiMultiClientGenerator(device.ctx, specs, device.clientStrategy, nil, f.server.URL,
			device.byJwt, f.space.platformUrl, "h1-owner", "test", "0", nil, clientSettings, apiSettings)
		createdGenerator.ApiMultiClientGenerator = api
		generator = createdGenerator
		return generator
	}
	jwt := testingRefreshableJwtWithMarker(t, "h1-owner-device")
	var err error
	device, err = newDeviceLocalWithOverrides(f.space, jwt, "h1-owner", "test", "0", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := device.CloseAndWait(ctx); err != nil {
			t.Error(err)
		}
	})
	// DNS upstream probes are unrelated to the H1 ownership boundary.
	upgrade := connect.DefaultUpgradeMuxSettings()
	upgrade.Dns = nil
	device.SetUpgradeMuxSettings(upgrade)
	device.SetTransportSettings(&TransportSettings{Mode: TransportModeH1})
	device.SetConnectLocation(&ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}})
	device.stateLock.Lock()
	multi, ok := device.remoteUserNatClient.(*connect.RemoteUserNatMultiClient)
	device.stateLock.Unlock()
	if !ok || generator == nil {
		t.Fatal("actual destination generation was not created")
	}
	return device, generator, multi
}

func h1OwnerTraffic(t *testing.T, device *DeviceLocal) {
	t.Helper()
	echoed := make(chan []byte, 1)
	unsub := device.AddReceivePacketCallback(func(_ connect.TransferPath, _ protocol.ProvideMode, _ *connect.IpPath, packet []byte) {
		_, payload, err := connect.ParseIpPathWithPayload(packet)
		if err == nil {
			select {
			case echoed <- bytes.Clone(payload):
			default:
			}
		}
	})
	defer unsub()
	for i := range 8 {
		payload := bytes.Repeat([]byte{byte(i + 1)}, 256)
		packet := craftIpv4Packet(connect.IpProtocolUdp, net.IPv4(10, 0, 0, 5), 41000+i, net.IPv4(203, 0, 113, 7), 123, false, payload)
		if !device.SendPacket(packet, int32(len(packet))) {
			t.Fatal("packet admission refused")
		}
		select {
		case got := <-echoed:
			if !bytes.Equal(got, payload) {
				t.Fatal("H1 payload mismatch")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no H1 echo")
		}
	}
}

func TestDeviceLocalH1OwnerLifecycle(t *testing.T) {
	f := newH1OwnerFixture(t)
	device, generator, multi := f.connectDevice(t)
	defer func() {
		if t.Failed() {
			f.mu.Lock()
			t.Logf("local fixture calls=%v sockets=%d bytes=%d", f.apiCalls, len(f.conns), f.wireBytes.Load())
			f.mu.Unlock()
			generator.mu.Lock()
			t.Logf("generated=%d current=%+v", len(generator.clients), multi.MemoryOwnerCensus())
			generator.mu.Unlock()
		}
	}()
	var before, active, after runtime.MemStats
	runtime.ReadMemStats(&before)
	baseGoroutines := runtime.NumGoroutine()
	started := time.Now()
	h1OwnerTraffic(t, device)
	trafficElapsed := time.Since(started)
	live := device.memoryOwnerCensus()
	if f.wireBytes.Load() == 0 || live.Client.Transfer.Clients != 1 || live.Client.Flows == 0 {
		t.Fatalf("fixture did not exercise H1/current owners: %+v", live.Client)
	}
	runtime.ReadMemStats(&active)
	activeGoroutines := runtime.NumGoroutine()
	closeStarted := time.Now()
	if err := device.CloseAndWait(f.ctx); err != nil {
		t.Fatal(err)
	}
	closeElapsed := time.Since(closeStarted)
	joined := multi.MemoryOwnerCensus()
	if joined.Flows != 0 || joined.Transfer.Clients != 0 {
		t.Fatalf("joined current topology remains: %+v", joined)
	}
	generator.mu.Lock()
	defer generator.mu.Unlock()
	for _, client := range generator.clients {
		owners := client.MemoryOwnerCensus()
		if owners.SendWorkers != 0 || owners.ReceiveWorkers != 0 || owners.PacingServices != 0 {
			t.Fatalf("joined generator retained worker owners: %+v", owners)
		}
	}
	for _, carrier := range generator.carriers {
		select {
		case <-carrier.Done():
		default:
			t.Fatal("joined DeviceLocal retained an externally owned H1 carrier")
		}
	}
	runtime.ReadMemStats(&after)
	// These are process-wide immediate snapshots, not post-GC retention
	// measurements: the test deliberately retains the closed clients so their
	// worker counters can be checked, and fixture/provider owners remain live.
	t.Logf("local H1 lifecycle: traffic=%s close=%s live-clients=%d live-flows=%d goroutines=%d->%d->%d heap=%d->%d->%d runtime=%d->%d->%d",
		trafficElapsed, closeElapsed, live.Client.Transfer.Clients, live.Client.Flows,
		baseGoroutines, activeGoroutines, runtime.NumGoroutine(), before.HeapAlloc, active.HeapAlloc, after.HeapAlloc,
		before.Sys-before.HeapReleased, active.Sys-active.HeapReleased, after.Sys-after.HeapReleased)
}

func TestDeviceLocalH1MigrationRetiresEveryCarrierGeneration(t *testing.T) {
	f := newH1OwnerFixture(t)
	device, generator, _ := f.connectDevice(t)
	h1OwnerTraffic(t, device)
	generator.mu.Lock()
	if len(generator.clients) != 1 || len(generator.carriers) != 1 {
		generator.mu.Unlock()
		t.Fatal("single-exit H1 fixture did not create one owned client/carrier")
	}
	client := generator.clients[0]
	old := generator.carriers[0]
	generator.mu.Unlock()
	generator.MigrateClientTransport(client, nil, time.Now())
	select {
	case <-old.Done():
	case <-f.ctx.Done():
		t.Fatal("old SDK H1 carrier did not retire after migration")
	}
	h1OwnerTraffic(t, device)
	if err := device.CloseAndWait(f.ctx); err != nil {
		t.Fatal(err)
	}
	generator.mu.Lock()
	defer generator.mu.Unlock()
	if len(generator.carriers) != 2 {
		t.Fatalf("expected exactly two H1 generations, got %d", len(generator.carriers))
	}
	for _, carrier := range generator.carriers {
		select {
		case <-carrier.Done():
		default:
			t.Fatal("DeviceLocal joined before an old/current carrier completed")
		}
	}
	if owners := device.memoryOwnerCensus(); owners.Client.Transfer.Clients != 0 {
		t.Fatalf("closed DeviceLocal retained indexed window clients: %+v", owners.Client)
	}
}
