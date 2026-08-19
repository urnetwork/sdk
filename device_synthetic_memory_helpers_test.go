package sdk

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// syntheticDeviceRpcTransport hard-wires a DeviceRemote to a DeviceLocal while
// retaining the production deviceRpcMux and its framing/byte budgets. Only the
// websocket implementation is synthetic: every WriteMessage makes the same
// ownership-separating copy a real websocket write requires, then hands the
// complete message to its peer.
type syntheticDeviceRpcTransport struct {
	ctx      context.Context
	cancel   context.CancelFunc
	settings *deviceRpcSettings
	accepts  chan [2]net.Conn
	close    sync.Once
}

func newSyntheticDeviceRpcTransport(
	ctx context.Context,
	settings *deviceRpcSettings,
) *syntheticDeviceRpcTransport {
	transportCtx, cancel := context.WithCancel(ctx)
	return &syntheticDeviceRpcTransport{
		ctx:      transportCtx,
		cancel:   cancel,
		settings: settings,
		accepts:  make(chan [2]net.Conn, 1),
	}
}

func (self *syntheticDeviceRpcTransport) Dial(
	ctx context.Context,
) (forward net.Conn, reverse net.Conn, err error) {
	sessionCtx, sessionCancel := context.WithCancel(self.ctx)
	remoteWs, localWs := newSyntheticDeviceRpcWsPair(sessionCtx, sessionCancel)
	remoteMux := newDeviceRpcMux(sessionCtx, remoteWs, self.settings)
	localMux := newDeviceRpcMux(sessionCtx, localWs, self.settings)

	// A DeviceRemote reconnect owns this synthetic session just as it owns a
	// real websocket. Cancelling the dial context tears down both mux halves.
	go func() {
		select {
		case <-ctx.Done():
			sessionCancel()
		case <-sessionCtx.Done():
		}
	}()

	localPair := [2]net.Conn{
		localMux.conns[deviceRpcStreamForward],
		localMux.conns[deviceRpcStreamReverse],
	}
	select {
	case self.accepts <- localPair:
		return remoteMux.conns[deviceRpcStreamForward],
			remoteMux.conns[deviceRpcStreamReverse], nil
	case <-ctx.Done():
		sessionCancel()
		return nil, nil, ctx.Err()
	case <-self.ctx.Done():
		sessionCancel()
		return nil, nil, self.ctx.Err()
	}
}

func (self *syntheticDeviceRpcTransport) Accept(
	ctx context.Context,
) (forward net.Conn, reverse net.Conn, err error) {
	select {
	case pair := <-self.accepts:
		return pair[0], pair[1], nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-self.ctx.Done():
		return nil, nil, self.ctx.Err()
	}
}

func (self *syntheticDeviceRpcTransport) Close() error {
	self.close.Do(self.cancel)
	return nil
}

var _ deviceRpcDialer = (*syntheticDeviceRpcTransport)(nil)
var _ deviceRpcListener = (*syntheticDeviceRpcTransport)(nil)

type syntheticDeviceRpcWsLink struct {
	ctx    context.Context
	cancel context.CancelFunc
	aToB   chan []byte
	bToA   chan []byte
}

type syntheticDeviceRpcWs struct {
	link     *syntheticDeviceRpcWsLink
	incoming <-chan []byte
	outgoing chan<- []byte
	local    syntheticDeviceRpcAddr
	remote   syntheticDeviceRpcAddr

	readLimit atomic.Int64
}

type syntheticDeviceRpcAddr string

func (self syntheticDeviceRpcAddr) Network() string { return "memory-device-rpc" }
func (self syntheticDeviceRpcAddr) String() string  { return string(self) }

func newSyntheticDeviceRpcWsPair(
	ctx context.Context,
	cancel context.CancelFunc,
) (*syntheticDeviceRpcWs, *syntheticDeviceRpcWs) {
	link := &syntheticDeviceRpcWsLink{
		ctx:    ctx,
		cancel: cancel,
		// The websocket is only a wire here. The production mux owns the
		// authoritative frame-count and byte bounds on both sides.
		aToB: make(chan []byte, 8),
		bToA: make(chan []byte, 8),
	}
	a := &syntheticDeviceRpcWs{
		link: link, incoming: link.bToA, outgoing: link.aToB,
		local: "device-remote", remote: "device-local",
	}
	b := &syntheticDeviceRpcWs{
		link: link, incoming: link.aToB, outgoing: link.bToA,
		local: "device-local", remote: "device-remote",
	}
	return a, b
}

func (self *syntheticDeviceRpcWs) WriteMessage(messageType int, data []byte) error {
	if messageType != DeviceRpcWsBinary {
		return fmt.Errorf("synthetic device rpc: unsupported message type %d", messageType)
	}
	message := bytes.Clone(data)
	select {
	case self.outgoing <- message:
		return nil
	case <-self.link.ctx.Done():
		return io.ErrClosedPipe
	}
}

func (self *syntheticDeviceRpcWs) WriteControl(
	messageType int,
	data []byte,
	_ time.Time,
) error {
	return self.WriteMessage(messageType, data)
}

func (self *syntheticDeviceRpcWs) Close() error {
	self.link.cancel()
	return nil
}

func (self *syntheticDeviceRpcWs) NextReader() (int, io.Reader, error) {
	select {
	case message := <-self.incoming:
		if limit := self.readLimit.Load(); 0 < limit && limit < int64(len(message)) {
			return 0, nil, fmt.Errorf(
				"synthetic device rpc frame is %d bytes, limit %d",
				len(message), limit,
			)
		}
		return DeviceRpcWsBinary, bytes.NewReader(message), nil
	case <-self.link.ctx.Done():
		return 0, nil, io.EOF
	}
}

func (self *syntheticDeviceRpcWs) SetReadLimit(limit int64) {
	self.readLimit.Store(limit)
}
func (*syntheticDeviceRpcWs) SetReadDeadline(time.Time) error   { return nil }
func (*syntheticDeviceRpcWs) SetWriteDeadline(time.Time) error  { return nil }
func (*syntheticDeviceRpcWs) SetPongHandler(func(string) error) {}
func (self *syntheticDeviceRpcWs) LocalAddr() net.Addr          { return self.local }
func (self *syntheticDeviceRpcWs) RemoteAddr() net.Addr         { return self.remote }

var _ deviceRpcWs = (*syntheticDeviceRpcWs)(nil)

// syntheticTunnelBridge mirrors PacketTunnelProvider's compact packet-batch
// ABI in both directions. Its Tun is the paired local IP stack standing in for
// NEPacketTunnelFlow; no test-only packet shortcut enters DeviceLocal.
type syntheticTunnelBridge struct {
	device *DeviceLocal
	tun    *connect.Tun
	sub    Sub

	wg        sync.WaitGroup
	closeOnce sync.Once

	sendBatchCount     atomic.Int64
	sendPacketCount    atomic.Int64
	receiveBatchCount  atomic.Int64
	receivePacketCount atomic.Int64
	droppedPacketCount atomic.Int64

	errMu sync.Mutex
	err   error
}

func newSyntheticTunnelBridge(
	t *testing.T,
	device *DeviceLocal,
) *syntheticTunnelBridge {
	t.Helper()

	tunSettings := connect.DefaultTunSettingsWithBufferSize(2048)
	tunSettings.Log = connect.NewNoopLogger()
	// NEPacketTunnelFlow injects the returned IP packets individually; it does
	// not apply the connect Tun's server-side GRO optimization. Keeping GRO off
	// here both matches the iOS consumer and avoids turning one native callback
	// batch into a synthetic super-segment burst the real client never sees.
	tunSettings.TcpGro = false
	// The synthetic exit has no geographic connect latency to hide. Launching
	// a second gVisor connection after the generic two-second stagger creates
	// an artificial half-open provider flow for every cold HTTPS dial and can
	// consume the tiny synthetic window while the winning flow transfers.
	tunSettings.DialRace = 1
	tunSettings.TcpSendBuffer = connect.TcpBufferRange{
		Min: 4 * 1024, Default: 32 * 1024, Max: 64 * 1024,
	}
	tunSettings.TcpReceiveBuffer = connect.TcpBufferRange{
		Min: 4 * 1024, Default: 32 * 1024, Max: 64 * 1024,
	}
	tun, err := connect.CreateTun(device.Ctx(), tunSettings)
	if err != nil {
		t.Fatalf("create paired synthetic tun: %v", err)
	}

	bridge := &syntheticTunnelBridge{device: device, tun: tun}
	// DeviceLocal's address is normally assigned to NEPacketTunnelNetworkSettings.
	// The in-process Tun reserves its own collision-free address; publish that
	// exact address from DeviceLocal so the synthetic pair has one identity too.
	device.stateLock.Lock()
	device.tunnelLocalAddress = tun.LocalAddresses()[0]
	device.stateLock.Unlock()

	bridge.sub = device.AddReceivePacketBatch(bridge)
	bridge.wg.Add(1)
	go bridge.pumpToDevice()
	return bridge
}

func (self *syntheticTunnelBridge) setError(err error) {
	if err == nil {
		return
	}
	self.errMu.Lock()
	if self.err == nil {
		self.err = err
	}
	self.errMu.Unlock()
}

func (self *syntheticTunnelBridge) Error() error {
	self.errMu.Lock()
	defer self.errMu.Unlock()
	return self.err
}

func encodedPacketCount(packetBatchBytes []byte) (int, bool) {
	count := 0
	for offset := 0; offset < len(packetBatchBytes); count++ {
		if len(packetBatchBytes)-offset < 2 {
			return 0, false
		}
		n := int(binary.BigEndian.Uint16(packetBatchBytes[offset : offset+2]))
		offset += 2
		if n == 0 || len(packetBatchBytes)-offset < n {
			return 0, false
		}
		offset += n
	}
	return count, 0 < count
}

func decodePacketBatchBorrowed(
	packetBatchBytes []byte,
	storage *[devicePacketBatchMaxPacketCount][]byte,
) ([][]byte, bool) {
	if len(packetBatchBytes) == 0 || devicePacketBatchMaxByteCount < len(packetBatchBytes) {
		return nil, false
	}
	offset := 0
	count := 0
	for offset < len(packetBatchBytes) {
		if devicePacketBatchMaxPacketCount <= count || len(packetBatchBytes)-offset < 2 {
			return nil, false
		}
		n := int(binary.BigEndian.Uint16(packetBatchBytes[offset : offset+2]))
		offset += 2
		if n == 0 || len(packetBatchBytes)-offset < n {
			return nil, false
		}
		storage[count] = packetBatchBytes[offset : offset+n]
		count++
		offset += n
	}
	return storage[:count], 0 < count
}

func (self *syntheticTunnelBridge) pumpToDevice() {
	defer self.wg.Done()
	packetStorage := make([][]byte, devicePacketBatchMaxPacketCount)
	for {
		packetCount, err := self.tun.ReadBatch(packetStorage)
		if err != nil {
			return
		}
		packets := packetStorage[:packetCount]
		emitted := withEncodedPacketBatches(packets, func(packetBatchBytes []byte) {
			batchPacketCount, ok := encodedPacketCount(packetBatchBytes)
			if !ok {
				self.setError(fmt.Errorf("encoded invalid packet batch"))
				return
			}
			self.sendBatchCount.Add(1)
			self.sendPacketCount.Add(int64(batchPacketCount))
			accepted := int(self.device.SendPacketBatch(packetBatchBytes))
			if accepted != batchPacketCount {
				self.droppedPacketCount.Add(int64(batchPacketCount - accepted))
			}
		})
		for _, packet := range packets {
			connect.MessagePoolReturn(packet)
		}
		if !emitted {
			self.setError(fmt.Errorf("failed to encode %d tunnel packets", packetCount))
		}
	}
}

func (self *syntheticTunnelBridge) ReceivePacketBatch(packetBatchBytes []byte) {
	var storage [devicePacketBatchMaxPacketCount][]byte
	packets, ok := decodePacketBatchBorrowed(packetBatchBytes, &storage)
	if !ok {
		self.setError(fmt.Errorf("device returned an invalid packet batch"))
		return
	}
	self.receiveBatchCount.Add(1)
	self.receivePacketCount.Add(int64(len(packets)))
	if _, err := self.tun.WriteBatch(packets); err != nil {
		self.setError(fmt.Errorf("write device packet batch to paired tun: %w", err))
	}
}

func (self *syntheticTunnelBridge) Close() {
	self.closeOnce.Do(func() {
		if self.sub != nil {
			self.sub.Close()
		}
		self.tun.Close()
		self.wg.Wait()
	})
}

var _ ReceivePacketBatch = (*syntheticTunnelBridge)(nil)

// syntheticProviderGenerator constructs the actual window Client used by
// DeviceLocal and pairs it to one provider Client over bounded in-memory
// routes. The generated client's queue sizes and shared byte budgets come from
// the measured DeviceLocal settings, matching ApiMultiClientGenerator wiring.
type syntheticProviderGenerator struct {
	providerClient *connect.Client
	deviceSettings *DeviceLocalSettings

	mu     sync.Mutex
	unsubs map[*connect.Client]func()
	closed bool

	createdClientCount atomic.Int64
}

func newSyntheticProviderGenerator(
	providerClient *connect.Client,
	deviceSettings *DeviceLocalSettings,
) *syntheticProviderGenerator {
	return &syntheticProviderGenerator{
		providerClient: providerClient,
		deviceSettings: deviceSettings,
		unsubs:         map[*connect.Client]func(){},
	}
}

func (self *syntheticProviderGenerator) NextDestinations(
	_ int,
	excludeDestinations []connect.MultiHopId,
	_ string,
) (map[connect.MultiHopId]connect.DestinationStats, error) {
	next := map[connect.MultiHopId]connect.DestinationStats{}
	for _, destination := range excludeDestinations {
		if 0 < destination.Len() && destination.Tail() == self.providerClient.ClientId() {
			return next, nil
		}
	}
	next[connect.RequireMultiHopId(self.providerClient.ClientId())] = connect.DestinationStats{}
	return next, nil
}

func (*syntheticProviderGenerator) NewClientArgs() (*connect.MultiClientGeneratorClientArgs, error) {
	return &connect.MultiClientGeneratorClientArgs{ClientId: connect.NewId()}, nil
}

func (*syntheticProviderGenerator) RemoveClientArgs(*connect.MultiClientGeneratorClientArgs) {}

func (self *syntheticProviderGenerator) RemoveClientWithArgs(
	client *connect.Client,
	_ *connect.MultiClientGeneratorClientArgs,
) {
	self.mu.Lock()
	unsub := self.unsubs[client]
	delete(self.unsubs, client)
	self.mu.Unlock()
	if unsub != nil {
		unsub()
	}
}

func (self *syntheticProviderGenerator) NewClientSettings() *connect.ClientSettings {
	settings := connect.DefaultClientSettingsWithBufferSize(self.deviceSettings.SequenceBufferSize)
	settings.Log = connect.NewNoopLogger()
	settings.SendBufferSettings.ResendQueueBudget =
		self.deviceSettings.ClientSettings.SendBufferSettings.ResendQueueBudget
	settings.ReceiveBufferSettings.ReceiveQueueBudget =
		self.deviceSettings.ClientSettings.ReceiveBufferSettings.ReceiveQueueBudget
	settings.WebRtcSettings.ReceiveBufferSize =
		self.deviceSettings.ClientSettings.WebRtcSettings.ReceiveBufferSize
	settings.WebRtcSettings.MemoryBudget =
		self.deviceSettings.ClientSettings.WebRtcSettings.MemoryBudget
	settings.WebRtcSettings.UseEgressOnlyIceInterfaces = true
	return settings
}

func (self *syntheticProviderGenerator) NewClient(
	ctx context.Context,
	args *connect.MultiClientGeneratorClientArgs,
	clientSettings *connect.ClientSettings,
) (*connect.Client, error) {
	client := connect.NewClient(
		ctx,
		args.ClientId,
		connect.NewNoContractClientOob(),
		clientSettings,
	)
	// Route channels are ownership hand-off edges, not transport queues. Keep
	// them unbuffered like the connect integration harnesses: the production
	// send/receive windows already provide bounded buffering, while an extra
	// channel queue can admit an artificial frame wave ahead of acknowledgments
	// and distort both flow control and the memory measurement.
	toProvider := make(chan []byte)
	toClient := make(chan []byte)
	clientSend := connect.NewSendGatewayTransport()
	clientReceive := connect.NewReceiveGatewayTransport()
	client.RouteManager().UpdateTransport(clientSend, []connect.Route{toProvider})
	client.RouteManager().UpdateTransport(clientReceive, []connect.Route{toClient})
	client.ContractManager().AddNoContractPeer(self.providerClient.ClientId())

	providerSend := connect.NewSendClientTransport(connect.DestinationId(client.ClientId()))
	providerReceive := connect.NewReceiveGatewayTransport()
	self.providerClient.RouteManager().UpdateTransport(providerReceive, []connect.Route{toProvider})
	self.providerClient.RouteManager().UpdateTransport(providerSend, []connect.Route{toClient})
	self.providerClient.ContractManager().AddNoContractPeer(client.ClientId())

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			client.RouteManager().RemoveTransport(clientSend)
			client.RouteManager().RemoveTransport(clientReceive)
			self.providerClient.RouteManager().RemoveTransport(providerReceive)
			self.providerClient.RouteManager().RemoveTransport(providerSend)
			client.Cancel()
		})
	}
	self.mu.Lock()
	if self.closed {
		self.mu.Unlock()
		unsub()
		return nil, fmt.Errorf("synthetic provider generator closed")
	}
	self.unsubs[client] = unsub
	self.mu.Unlock()
	self.createdClientCount.Add(1)
	return client, nil
}

func (*syntheticProviderGenerator) FixedDestinationSize() (int, bool) {
	return 1, true
}

func (self *syntheticProviderGenerator) Close() {
	self.mu.Lock()
	self.closed = true
	unsubs := make([]func(), 0, len(self.unsubs))
	for client, unsub := range self.unsubs {
		delete(self.unsubs, client)
		unsubs = append(unsubs, unsub)
	}
	self.mu.Unlock()
	for _, unsub := range unsubs {
		unsub()
	}
}

// syntheticEndpointRouter is installed in the provider LocalUserNat. It maps
// public-looking destination ports to loopback test endpoints and fails every
// other dial. Consequently the workload cannot touch the host's real network,
// and an unexpected dial proves a supposedly blocked flow reached the exit.
type syntheticEndpointRouter struct {
	targets map[int]string

	mu              sync.Mutex
	dialsByPort     map[int]int64
	unexpectedDials int64
}

func newSyntheticEndpointRouter(targets map[int]string) *syntheticEndpointRouter {
	return &syntheticEndpointRouter{
		targets:     targets,
		dialsByPort: map[int]int64{},
	}
}

func (self *syntheticEndpointRouter) DialContext(
	ctx context.Context,
	network string,
	address string,
) (net.Conn, error) {
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil, err
	}
	self.mu.Lock()
	self.dialsByPort[port]++
	target, ok := self.targets[port]
	if !ok {
		self.unexpectedDials++
	}
	self.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("synthetic exit has no endpoint for %s", address)
	}
	return (&net.Dialer{}).DialContext(ctx, network, target)
}

func (self *syntheticEndpointRouter) Dials(port int) int64 {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.dialsByPort[port]
}

func (self *syntheticEndpointRouter) UnexpectedDials() int64 {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.unexpectedDials
}

type syntheticEndpointServers struct {
	httpServer  *httptest.Server
	httpsServer *httptest.Server
	smtp465     *syntheticSmtpServer
	smtp587     *syntheticSmtpServer

	webRequests atomic.Int64
	webBytes    atomic.Int64
}

// Several concurrent 48 KiB objects approximate the median mobile page's
// independent resources without letting the in-process provider window's
// intentionally conservative cold-start pacing turn this memory test into a
// throughput timeout test.
const syntheticWebResponseByteCount = 48 * 1024
const syntheticHttpResponseByteCount = 4 * 1024

func newSyntheticEndpointServers(t *testing.T) *syntheticEndpointServers {
	t.Helper()
	servers := &syntheticEndpointServers{}
	httpsBody := bytes.Repeat([]byte("w"), syntheticWebResponseByteCount)
	httpBody := bytes.Repeat([]byte("h"), syntheticHttpResponseByteCount)
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body := httpsBody
		if request.TLS == nil {
			// Plain HTTP is represented by a small redirect/bootstrap-sized
			// object; HTTPS carries the realistic bulk response.
			body = httpBody
		}
		servers.webRequests.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		for offset := 0; offset < len(body); offset += 16 * 1024 {
			end := min(len(body), offset+16*1024)
			n, _ := w.Write(body[offset:end])
			servers.webBytes.Add(int64(n))
		}
	})
	servers.httpServer = httptest.NewServer(handler)
	servers.httpsServer = httptest.NewTLSServer(handler)

	certPem, keyPem, err := generateSelfSignedCert()
	if err != nil {
		servers.Close()
		t.Fatalf("generate SMTP TLS certificate: %v", err)
	}
	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		servers.Close()
		t.Fatalf("parse SMTP TLS certificate: %v", err)
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	servers.smtp465 = newSyntheticSmtpServer(t, tlsConfig, true)
	servers.smtp587 = newSyntheticSmtpServer(t, tlsConfig, false)
	return servers
}

func (self *syntheticEndpointServers) Targets() map[int]string {
	return map[int]string{
		80:  self.httpServer.Listener.Addr().String(),
		443: self.httpsServer.Listener.Addr().String(),
		465: self.smtp465.Addr(),
		587: self.smtp587.Addr(),
	}
}

func (self *syntheticEndpointServers) Close() {
	if self.httpServer != nil {
		self.httpServer.Close()
	}
	if self.httpsServer != nil {
		self.httpsServer.Close()
	}
	if self.smtp465 != nil {
		self.smtp465.Close()
	}
	if self.smtp587 != nil {
		self.smtp587.Close()
	}
}

type syntheticSmtpServer struct {
	listener    net.Listener
	tlsConfig   *tls.Config
	implicitTls bool

	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	close   sync.Once
	connsMu sync.Mutex
	conns   map[net.Conn]bool

	accepted     atomic.Int64
	submissions  atomic.Int64
	messageBytes atomic.Int64
	dataBytes    atomic.Int64
}

func newSyntheticSmtpServer(
	t *testing.T,
	tlsConfig *tls.Config,
	implicitTls bool,
) *syntheticSmtpServer {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen synthetic SMTP endpoint: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := &syntheticSmtpServer{
		listener: listener, tlsConfig: tlsConfig.Clone(), implicitTls: implicitTls,
		ctx: ctx, cancel: cancel, conns: map[net.Conn]bool{},
	}
	server.wg.Add(1)
	go server.acceptLoop()
	return server
}

func (self *syntheticSmtpServer) Addr() string { return self.listener.Addr().String() }

func (self *syntheticSmtpServer) acceptLoop() {
	defer self.wg.Done()
	for {
		conn, err := self.listener.Accept()
		if err != nil {
			return
		}
		self.accepted.Add(1)
		self.connsMu.Lock()
		self.conns[conn] = true
		self.connsMu.Unlock()
		self.wg.Add(1)
		go func() {
			defer self.wg.Done()
			defer func() {
				self.connsMu.Lock()
				delete(self.conns, conn)
				self.connsMu.Unlock()
				conn.Close()
			}()
			// A freshly formed provider window deliberately starts with small
			// congestion/receive windows. The first synthetic transfer can take
			// materially longer under the race detector or a 32 MiB GC limit; the
			// workload's own context remains the authoritative hang bound.
			conn.SetDeadline(time.Now().Add(2 * time.Minute))
			if self.implicitTls {
				self.serveImplicitTls(conn)
			} else {
				self.serveStartTls(conn)
			}
		}()
	}
}

func (self *syntheticSmtpServer) serveImplicitTls(raw net.Conn) {
	conn := tls.Server(raw, self.tlsConfig)
	if err := conn.Handshake(); err != nil {
		return
	}
	self.serveSubmission(conn, true)
}

func (self *syntheticSmtpServer) serveStartTls(raw net.Conn) {
	reader := bufio.NewReader(raw)
	if _, err := io.WriteString(raw, "220 synthetic.local ESMTP ready\r\n"); err != nil {
		return
	}
	line, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "EHLO ") {
		return
	}
	if _, err := io.WriteString(raw, "250-synthetic.local\r\n250 STARTTLS\r\n"); err != nil {
		return
	}
	line, err = reader.ReadString('\n')
	if err != nil || strings.ToUpper(strings.TrimSpace(line)) != "STARTTLS" {
		return
	}
	if _, err := io.WriteString(raw, "220 2.0.0 begin TLS\r\n"); err != nil {
		return
	}
	conn := tls.Server(raw, self.tlsConfig)
	if err := conn.Handshake(); err != nil {
		return
	}
	self.serveSubmission(conn, false)
}

func (self *syntheticSmtpServer) serveSubmission(conn net.Conn, greeting bool) {
	reader := bufio.NewReader(conn)
	if greeting {
		if _, err := io.WriteString(conn, "220 synthetic.local ESMTP ready\r\n"); err != nil {
			return
		}
	}
	inData := false
	messageBytes := int64(0)
	for commandCount := 0; commandCount < 256; commandCount++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if inData {
			if trimmed == "." {
				self.submissions.Add(1)
				self.messageBytes.Add(messageBytes)
				if _, err := io.WriteString(conn, "250 2.0.0 queued\r\n"); err != nil {
					return
				}
				inData = false
				continue
			}
			messageBytes += int64(len(line))
			self.dataBytes.Add(int64(len(line)))
			continue
		}
		upper := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(upper, "EHLO "):
			if _, err := io.WriteString(conn, "250-synthetic.local\r\n250 AUTH PLAIN\r\n"); err != nil {
				return
			}
		case strings.HasPrefix(upper, "AUTH PLAIN"):
			if _, err := io.WriteString(conn, "235 2.7.0 authenticated\r\n"); err != nil {
				return
			}
		case strings.HasPrefix(upper, "MAIL FROM:"):
			if _, err := io.WriteString(conn, "250 2.1.0 sender ok\r\n"); err != nil {
				return
			}
		case strings.HasPrefix(upper, "RCPT TO:"):
			if _, err := io.WriteString(conn, "250 2.1.5 recipient ok\r\n"); err != nil {
				return
			}
		case upper == "DATA":
			if _, err := io.WriteString(conn, "354 send message\r\n"); err != nil {
				return
			}
			inData = true
			messageBytes = 0
		case upper == "QUIT":
			_, _ = io.WriteString(conn, "221 2.0.0 bye\r\n")
			return
		default:
			if _, err := io.WriteString(conn, "500 5.5.1 unsupported\r\n"); err != nil {
				return
			}
		}
	}
}

func (self *syntheticSmtpServer) Close() {
	self.close.Do(func() {
		self.cancel()
		self.listener.Close()
		self.connsMu.Lock()
		for conn := range self.conns {
			conn.Close()
		}
		self.connsMu.Unlock()
		self.wg.Wait()
	})
}

func syntheticReadSmtpResponse(reader *bufio.Reader, expected int) error {
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if len(line) < 4 {
			return fmt.Errorf("short SMTP response %q", line)
		}
		code, err := strconv.Atoi(line[:3])
		if err != nil || code != expected {
			return fmt.Errorf("SMTP response %q, want %d", strings.TrimSpace(line), expected)
		}
		if line[3] == ' ' {
			return nil
		}
		if line[3] != '-' {
			return fmt.Errorf("malformed SMTP response %q", strings.TrimSpace(line))
		}
	}
}

func syntheticWriteSmtpCommand(
	conn net.Conn,
	reader *bufio.Reader,
	expected int,
	command string,
) error {
	if _, err := io.WriteString(conn, command+"\r\n"); err != nil {
		return err
	}
	return syntheticReadSmtpResponse(reader, expected)
}

func syntheticMailBody(byteCount int) []byte {
	var body bytes.Buffer
	line := strings.Repeat("m", 74) + "\r\n"
	for body.Len()+len(line) <= byteCount {
		body.WriteString(line)
	}
	if body.Len() < byteCount {
		body.WriteString(strings.Repeat("m", byteCount-body.Len()))
		body.WriteString("\r\n")
	}
	return body.Bytes()
}

func runSyntheticSubmission(
	ctx context.Context,
	tun *connect.Tun,
	port int,
	startTls bool,
	body []byte,
) error {
	address := net.JoinHostPort("203.0.113.25", strconv.Itoa(port))
	raw, err := tun.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("dial SMTP/%d: %w", port, err)
	}
	defer raw.Close()
	deadline := time.Now().Add(30 * time.Second)
	if raceEnabled {
		deadline = time.Now().Add(120 * time.Second)
	}
	raw.SetDeadline(deadline)

	var conn net.Conn = raw
	if startTls {
		reader := bufio.NewReader(raw)
		if err := syntheticReadSmtpResponse(reader, 220); err != nil {
			return fmt.Errorf("SMTP/587 greeting: %w", err)
		}
		if err := syntheticWriteSmtpCommand(raw, reader, 250, "EHLO device.local"); err != nil {
			return fmt.Errorf("SMTP/587 EHLO: %w", err)
		}
		if err := syntheticWriteSmtpCommand(raw, reader, 220, "STARTTLS"); err != nil {
			return fmt.Errorf("SMTP/587 STARTTLS: %w", err)
		}
	}
	tlsConn := tls.Client(raw, &tls.Config{
		InsecureSkipVerify: true, // pinned to a loopback-only test endpoint
		MinVersion:         tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("SMTP/%d TLS handshake: %w", port, err)
	}
	conn = tlsConn
	reader := bufio.NewReader(conn)
	if !startTls {
		if err := syntheticReadSmtpResponse(reader, 220); err != nil {
			return fmt.Errorf("SMTP/465 greeting: %w", err)
		}
	}
	commands := []struct {
		command  string
		expected int
	}{
		{"EHLO device.local", 250},
		{"AUTH PLAIN AHVzZXIAcGFzcw==", 235},
		{"MAIL FROM:<sender@example.test>", 250},
		{"RCPT TO:<recipient@example.test>", 250},
		{"DATA", 354},
	}
	for _, command := range commands {
		if err := syntheticWriteSmtpCommand(conn, reader, command.expected, command.command); err != nil {
			return fmt.Errorf("SMTP/%d %s: %w", port, command.command, err)
		}
	}
	if _, err := io.Copy(conn, bytes.NewReader(body)); err != nil {
		return fmt.Errorf("SMTP/%d message body: %w", port, err)
	}
	if _, err := io.WriteString(conn, ".\r\n"); err != nil {
		return fmt.Errorf("SMTP/%d message terminator: %w", port, err)
	}
	if err := syntheticReadSmtpResponse(reader, 250); err != nil {
		return fmt.Errorf("SMTP/%d queue response: %w", port, err)
	}
	if err := syntheticWriteSmtpCommand(conn, reader, 221, "QUIT"); err != nil {
		return fmt.Errorf("SMTP/%d QUIT: %w", port, err)
	}
	return nil
}

// syntheticExitProvider is the fixed remote provider selected by DeviceLocal's
// window. It runs the production provider NAT and provider-side security policy;
// only its socket dialer is replaced so the test remains hermetic.
type syntheticExitProvider struct {
	client    *connect.Client
	localNat  *connect.LocalUserNat
	provider  *connect.RemoteUserNatProvider
	generator *syntheticProviderGenerator
	router    *syntheticEndpointRouter

	closeOnce sync.Once
}

func newSyntheticExitProvider(
	ctx context.Context,
	deviceSettings *DeviceLocalSettings,
	targets map[int]string,
) *syntheticExitProvider {
	clientSettings := connect.DefaultClientSettingsWithBufferSize(deviceSettings.SequenceBufferSize)
	clientSettings.Log = connect.NewNoopLogger()
	clientSettings.WebRtcSettings.UseEgressOnlyIceInterfaces = true
	client := connect.NewClient(
		ctx,
		connect.NewId(),
		connect.NewNoContractClientOob(),
		clientSettings,
	)
	client.ContractManager().SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Public:           true,
		protocol.ProvideMode_FriendsAndFamily: true,
		protocol.ProvideMode_Network:          true,
	})

	router := newSyntheticEndpointRouter(targets)
	// Size the synthetic exit like DeviceLocal's mobile provider share. The
	// socket flow limits and queues are therefore part of the measured profile.
	natSettings := connect.DefaultProviderLocalUserNatSettingsWithMemoryTarget(4 * 1024 * 1024)
	natSettings.Log = connect.NewNoopLogger()
	dialSettings := &connect.DialContextSettings{DialContext: router.DialContext}
	natSettings.TcpBufferSettings.DialContextSettings = dialSettings
	natSettings.UdpBufferSettings.DialContextSettings = dialSettings
	localNat := connect.NewLocalUserNat(ctx, "synthetic-device-exit", natSettings)

	providerSettings := connect.DefaultRemoteUserNatProviderSettings()
	providerSettings.EventEpoch = 50 * time.Millisecond
	provider := connect.NewRemoteUserNatProvider(client, localNat, providerSettings)

	return &syntheticExitProvider{
		client:    client,
		localNat:  localNat,
		provider:  provider,
		generator: newSyntheticProviderGenerator(client, deviceSettings),
		router:    router,
	}
}

func (self *syntheticExitProvider) Close() {
	self.closeOnce.Do(func() {
		self.generator.Close()
		self.provider.Close()
		self.localNat.Close()
		self.client.Close()
	})
}

func runSyntheticWebRequest(
	ctx context.Context,
	tun *connect.Tun,
	secure bool,
	path string,
) error {
	port := 80
	if secure {
		port = 443
	}
	raw, err := tun.DialContext(
		ctx,
		"tcp",
		net.JoinHostPort("203.0.113.44", strconv.Itoa(port)),
	)
	if err != nil {
		return err
	}
	defer raw.Close()
	deadline := time.Now().Add(30 * time.Second)
	if raceEnabled {
		deadline = time.Now().Add(2 * time.Minute)
	}
	if err := raw.SetDeadline(deadline); err != nil {
		return err
	}

	var conn net.Conn = raw
	if secure {
		tlsConn := tls.Client(raw, &tls.Config{
			InsecureSkipVerify: true, // pinned to a loopback-only test endpoint
			MinVersion:         tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return err
		}
		conn = tlsConn
	}
	if _, err := fmt.Fprintf(
		conn,
		"GET %s HTTP/1.1\r\nHost: web.synthetic.test\r\nConnection: close\r\n\r\n",
		path,
	); err != nil {
		return err
	}
	response, err := http.ReadResponse(
		bufio.NewReader(conn),
		&http.Request{Method: http.MethodGet},
	)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", response.Status)
	}
	n, err := io.Copy(io.Discard, response.Body)
	if err != nil {
		return fmt.Errorf(
			"read response body after %d/%d bytes: %w",
			n,
			response.ContentLength,
			err,
		)
	}
	if response.ContentLength <= 0 || n != response.ContentLength {
		return fmt.Errorf("web response bytes = %d, want content-length %d", n, response.ContentLength)
	}
	return nil
}

// runSyntheticBlockedTraffic emits fresh DHT flows through the paired TUN.
// The client security policy must reject them before the synthetic provider;
// the exit router records any escape as an unexpected dial.
func runSyntheticBlockedTraffic(ctx context.Context, tun *connect.Tun, flowCount int) error {
	for range flowCount {
		conn, err := tun.DialContext(ctx, "udp", "203.0.113.66:51415")
		if err != nil {
			return fmt.Errorf("dial blocked DHT flow: %w", err)
		}
		if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			conn.Close()
			return err
		}
		if _, err := conn.Write(bittorrentDhtPing()); err != nil {
			conn.Close()
			return fmt.Errorf("write blocked DHT flow: %w", err)
		}
		if err := conn.Close(); err != nil {
			return err
		}
	}
	return nil
}
