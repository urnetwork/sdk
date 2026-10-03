package sdk

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// HOW THE APP REACHES A MESSAGE SERVER THAT IS ITS OWN HOST.
//
// [MessageClient] makes the app a connect client of the operator, and the operator delivers every
// frame to the server's client_id, so the operator carries, and today can read, every request
// (ledger 266, 267). This is the other path: the server listens on its own TLS endpoint
// (msgrepo/endpoint) and the app opens a TLS session to it, carrying the same §4.2 frames, one per
// WebSocket message. The transport above ([MessageTransport]) cannot tell the difference, which
// is the point: Hello, fragmentation and request correlation are the code that already runs.
//
// TWO WAYS TO GET THERE, AND THE APP CHOOSES.
//
//   - [MessageRouteUrnetwork], the default: the TCP connection is dialled through
//     [messageTunnel], which sends it out of a URnetwork exit provider. The server sees the exit's
//     address and never this device's. The operator relays sealed packets to the exit and learns
//     that this device uses the mesh, how much and when, and nothing about where it goes.
//   - [MessageRouteDirect], the fallback the owner asked for: an ordinary TCP connection, which
//     shows this device's address to the message server. It exists for when the mesh is the
//     problem.
//
// Either way the server is authenticated by its KEY: the TLS session is accepted only if the
// certificate's SubjectPublicKeyInfo hashes to [MessageRouteConfig.Pin]. No CA, no domain name and
// no operator is trusted to vouch for it, which is what closes 266's "nothing authenticates the
// message server to the app" for this path.

type MessageRouteMode int

const (
	MessageRouteUrnetwork MessageRouteMode = 0
	MessageRouteDirect    MessageRouteMode = 1
)

func (self MessageRouteMode) String() string {
	switch self {
	case MessageRouteUrnetwork:
		return "urnetwork"
	case MessageRouteDirect:
		return "direct"
	}
	return fmt.Sprintf("mode(%d)", int(self))
}

// ParseMessageRouteMode reads the setting as the app stores it. Empty is the default.
func ParseMessageRouteMode(value string) (MessageRouteMode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "urnetwork":
		return MessageRouteUrnetwork, nil
	case "direct":
		return MessageRouteDirect, nil
	}
	return 0, fmt.Errorf("%w: %q is not \"urnetwork\" or \"direct\"", ErrMessageRouteBadMode, value)
}

var (
	ErrMessageRouteBadMode     = errors.New("sdk: unknown message route mode")
	ErrMessageRouteNoEndpoint  = errors.New("sdk: a message route needs the server's endpoint, a wss:// url")
	ErrMessageRouteBadEndpoint = errors.New("sdk: the message server endpoint is not a wss:// url with a host")
	ErrMessageRouteBadPin      = errors.New("sdk: the message server pin is not 32 octets of SHA-256 (64 hex characters)")
	// The TLS session was refused because the server's key is not the pinned one. Somebody other
	// than the server answered: an exit, a network, or a server whose key was replaced.
	ErrMessageRoutePinMismatch = errors.New("sdk: the message server presented a key this app was not told to expect")
	errMessageRouteClosed      = errors.New("sdk: the message route is closed")
)

const (
	messageRouteMinBackoff       = 1 * time.Second
	messageRouteMaxBackoff       = 30 * time.Second
	messageRouteDialTimeout      = 60 * time.Second
	messageRouteHandshakeTimeout = 30 * time.Second
	messageRoutePingInterval     = 20 * time.Second
	messageRouteWriteTimeout     = 30 * time.Second
	messageRouteMaxMessageBytes  = int64(1 << 20)
	messageRouteOutboundDepth    = 1024
)

// ParseMessageRoutePin reads a pin as 64 hex characters, optionally prefixed "sha256/".
func ParseMessageRoutePin(value string) ([]byte, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "sha256/")
	pin, err := hex.DecodeString(value)
	if err != nil || len(pin) != sha256.Size {
		return nil, ErrMessageRouteBadPin
	}
	return pin, nil
}

// MessageRouteConfig is one route to one message server.
type MessageRouteConfig struct {
	// The server's endpoint, e.g. "wss://74.50.11.53/urmessage/v1". An IP address sends no TLS
	// server name, so an exit sees an address and a port and no name.
	Endpoint string
	// SHA-256 of the server certificate's SubjectPublicKeyInfo.
	Pin []byte

	Mode MessageRouteMode

	// For [MessageRouteUrnetwork]: the credential the tunnel's window clients are minted from,
	// and the operator they are minted on. A CREDENTIAL: never logged by this package.
	ByClientJwt string
	Host        string
	Env         string
	PlatformUrl string
	ApiUrl      string
	AppVersion  string

	// A test seam. When set it replaces the dialler of either mode.
	DialContext func(ctx context.Context, network string, address string) (net.Conn, error)
}

// MessageRouteStatus is what the app shows about its route.
type MessageRouteStatus struct {
	Mode      string `json:"mode"`
	Connected bool   `json:"connected"`
	// sessions established since this client was made; above 1 means it has reconnected
	Connects  int    `json:"connects"`
	LastError string `json:"last_error,omitempty"`
	// [MessageRouteUrnetwork] only: the exit providers the tunnel's window holds
	WindowProviders int      `json:"window_providers"`
	WindowCountries []string `json:"window_countries,omitempty"`
}

type messageRouteOutbound struct {
	frame *protocol.Frame
	ack   connect.AckFunction
}

// MessageRouteClient satisfies [MessageTransportClient], so [NewMessageTransport] takes it where it
// takes a [MessageClient].
type MessageRouteClient struct {
	ctx    context.Context
	cancel context.CancelFunc

	mode      MessageRouteMode
	endpoint  string
	pin       []byte
	netDial   func(ctx context.Context, network string, address string) (net.Conn, error)
	tunnel    *messageTunnel
	outbound  chan messageRouteOutbound
	wg        sync.WaitGroup
	closeOnce sync.Once

	mutex        sync.Mutex
	callbacks    map[uint64]connect.ReceiveFunction
	nextCallback uint64
	replaced     map[uint64]func()
	connected    bool
	connects     int
	lastError    string
}

var _ MessageTransportClient = (*MessageRouteClient)(nil)

// NewMessageRouteClient starts dialling at once and keeps a session open, redialling with backoff,
// until [MessageRouteClient.Close]. It does not wait: the first [MessageTransport.Hello] is what
// finds out whether the server answered.
func NewMessageRouteClient(ctx context.Context, config *MessageRouteConfig) (*MessageRouteClient, error) {
	if config == nil || strings.TrimSpace(config.Endpoint) == "" {
		return nil, ErrMessageRouteNoEndpoint
	}
	endpoint, err := url.Parse(strings.TrimSpace(config.Endpoint))
	if err != nil || endpoint.Scheme != "wss" || endpoint.Hostname() == "" {
		return nil, ErrMessageRouteBadEndpoint
	}
	if len(config.Pin) != sha256.Size {
		return nil, ErrMessageRouteBadPin
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	self := &MessageRouteClient{
		ctx:       cancelCtx,
		cancel:    cancel,
		mode:      config.Mode,
		endpoint:  endpoint.String(),
		pin:       append([]byte(nil), config.Pin...),
		outbound:  make(chan messageRouteOutbound, messageRouteOutboundDepth),
		callbacks: map[uint64]connect.ReceiveFunction{},
		replaced:  map[uint64]func(){},
	}

	switch {
	case config.DialContext != nil:
		self.netDial = config.DialContext
	case config.Mode == MessageRouteDirect:
		dialer := &net.Dialer{Timeout: messageRouteDialTimeout}
		self.netDial = dialer.DialContext
	case config.Mode == MessageRouteUrnetwork:
		tunnel, err := self.newTunnel(cancelCtx, config)
		if err != nil {
			cancel()
			return nil, err
		}
		self.tunnel = tunnel
		self.netDial = tunnel.DialContext
	default:
		cancel()
		return nil, fmt.Errorf("%w: %d", ErrMessageRouteBadMode, int(config.Mode))
	}

	self.wg.Add(1)
	go connect.HandleError(self.run)
	return self, nil
}

func (self *MessageRouteClient) newTunnel(ctx context.Context, config *MessageRouteConfig) (*messageTunnel, error) {
	byJwt := strings.TrimSpace(config.ByClientJwt)
	if byJwt == "" {
		return nil, ErrMessageClientNoJwt
	}
	parsed, err := connect.ParseByJwtUnverified(byJwt)
	if err != nil {
		// the token is not in this message; see NewMessageClient
		return nil, fmt.Errorf("sdk: this by_client_jwt could not be parsed: %w", err)
	}
	if parsed.ClientId == (connect.Id{}) {
		return nil, ErrMessageClientNoClientId
	}
	platformUrl, apiUrl, err := messageClientUrls(&MessageClientConfig{
		Host:        config.Host,
		Env:         config.Env,
		PlatformUrl: config.PlatformUrl,
		ApiUrl:      config.ApiUrl,
	})
	if err != nil {
		return nil, err
	}
	appVersion := config.AppVersion
	if appVersion == "" {
		appVersion = messageClientDefaultAppVersion
	}
	return newMessageTunnel(ctx, &messageTunnelConfig{
		ByClientJwt: byJwt,
		ClientId:    parsed.ClientId,
		ApiUrl:      apiUrl,
		PlatformUrl: platformUrl,
		AppVersion:  appVersion,
	})
}

func (self *MessageRouteClient) Mode() MessageRouteMode {
	return self.mode
}

func (self *MessageRouteClient) Status() *MessageRouteStatus {
	self.mutex.Lock()
	status := &MessageRouteStatus{
		Mode:      self.mode.String(),
		Connected: self.connected,
		Connects:  self.connects,
		LastError: self.lastError,
	}
	self.mutex.Unlock()
	if self.tunnel != nil {
		status.WindowProviders, status.WindowCountries = self.tunnel.window()
	}
	return status
}

// ── the session ────────────────────────────────────────────────────────────────────────────

func (self *MessageRouteClient) run() {
	defer self.wg.Done()
	backoff := messageRouteMinBackoff
	for {
		if self.ctx.Err() != nil {
			return
		}
		ws, err := self.dial()
		if err != nil {
			self.setError(err)
			if !self.sleep(backoff) {
				return
			}
			backoff = min(2*backoff, messageRouteMaxBackoff)
			continue
		}
		backoff = messageRouteMinBackoff
		self.mutex.Lock()
		self.connected = true
		self.connects += 1
		self.lastError = ""
		var replaced []func()
		if 1 < self.connects {
			// a second session is a new connection at the server, with no Hello on it
			for _, callback := range self.replaced {
				replaced = append(replaced, callback)
			}
		}
		self.mutex.Unlock()
		for _, callback := range replaced {
			callback()
		}

		err = self.serve(ws)

		self.mutex.Lock()
		self.connected = false
		self.mutex.Unlock()
		if err != nil {
			self.setError(err)
		}
		if !self.sleep(messageRouteMinBackoff) {
			return
		}
	}
}

func (self *MessageRouteClient) tlsConfig() *tls.Config {
	pin := self.pin
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"http/1.1"},
		// the pin below IS the verification: the chain is not consulted, the key is
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return ErrMessageRoutePinMismatch
			}
			presented := sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if subtle.ConstantTimeCompare(presented[:], pin) != 1 {
				return ErrMessageRoutePinMismatch
			}
			return nil
		},
	}
}

func (self *MessageRouteClient) dial() (*websocket.Conn, error) {
	dialer := &websocket.Dialer{
		NetDialContext:   self.netDial,
		TLSClientConfig:  self.tlsConfig(),
		HandshakeTimeout: messageRouteHandshakeTimeout,
		ReadBufferSize:   16 * 1024,
		WriteBufferSize:  16 * 1024,
	}
	ctx, cancel := context.WithTimeout(self.ctx, messageRouteDialTimeout)
	defer cancel()
	ws, _, err := dialer.DialContext(ctx, self.endpoint, nil)
	if err != nil {
		return nil, err
	}
	return ws, nil
}

// serve pumps one session until it fails or the client closes. Frames still queued when it ends
// stay queued for the next session; only a frame whose write failed is given up, with its ack.
func (self *MessageRouteClient) serve(ws *websocket.Conn) error {
	readerDone := make(chan error, 1)
	go connect.HandleError(func() {
		readerDone <- self.readLoop(ws)
	})
	finish := func(err error) error {
		ws.Close()
		<-readerDone
		return err
	}

	ping := time.NewTicker(messageRoutePingInterval)
	defer ping.Stop()
	for {
		select {
		case <-self.ctx.Done():
			ws.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
			return finish(nil)
		case err := <-readerDone:
			ws.Close()
			return err
		case <-ping.C:
			if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(messageRouteWriteTimeout)); err != nil {
				return finish(err)
			}
		case item := <-self.outbound:
			data, err := proto.Marshal(item.frame)
			connect.MessagePoolReturn(item.frame.MessageBytes)
			if err != nil {
				if item.ack != nil {
					item.ack(err)
				}
				continue
			}
			ws.SetWriteDeadline(time.Now().Add(messageRouteWriteTimeout))
			if err := ws.WriteMessage(websocket.BinaryMessage, data); err != nil {
				if item.ack != nil {
					item.ack(err)
				}
				return finish(err)
			}
			if item.ack != nil {
				item.ack(nil)
			}
		}
	}
}

func (self *MessageRouteClient) readLoop(ws *websocket.Conn) error {
	ws.SetReadLimit(messageRouteMaxMessageBytes)
	idle := 3 * messageRoutePingInterval
	extend := func() error {
		return ws.SetReadDeadline(time.Now().Add(idle))
	}
	extend()
	ws.SetPongHandler(func(string) error {
		return extend()
	})
	ws.SetPingHandler(func(data string) error {
		extend()
		err := ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(messageRouteWriteTimeout))
		if errors.Is(err, websocket.ErrCloseSent) {
			return nil
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil
		}
		return err
	})
	for {
		kind, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		extend()
		if kind != websocket.BinaryMessage {
			continue
		}
		frame := &protocol.Frame{}
		if proto.Unmarshal(data, frame) != nil {
			continue
		}
		for _, callback := range self.callbackList() {
			callback(connect.TransferPath{}, []*protocol.Frame{frame}, connect.Peer{})
		}
	}
}

func (self *MessageRouteClient) sleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-self.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (self *MessageRouteClient) setError(err error) {
	if err == nil {
		return
	}
	message := err.Error()
	if errors.Is(err, ErrMessageRoutePinMismatch) {
		message = ErrMessageRoutePinMismatch.Error()
	}
	self.mutex.Lock()
	self.lastError = message
	self.mutex.Unlock()
}

func (self *MessageRouteClient) callbackList() []connect.ReceiveFunction {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	callbacks := make([]connect.ReceiveFunction, 0, len(self.callbacks))
	for _, callback := range self.callbacks {
		callbacks = append(callbacks, callback)
	}
	return callbacks
}

// ── MessageTransportClient ─────────────────────────────────────────────────────────────────

// SendWithTimeout queues a frame for the session, with connect's ownership rule: true means the
// frame was taken and its ack will fire once it is written or given up; false means it was not
// taken and its bytes are still the caller's. A negative timeout waits for room in the queue
// until the client closes, which is connect's own meaning of -1. The destination is not read:
// one route reaches one server.
func (self *MessageRouteClient) SendWithTimeout(
	frame *protocol.Frame,
	destination connect.Id,
	ackCallback connect.AckFunction,
	timeout time.Duration,
	opts ...any,
) bool {
	item := messageRouteOutbound{frame: frame, ack: ackCallback}
	if self.ctx.Err() != nil {
		return false
	}
	switch {
	case timeout < 0:
		select {
		case self.outbound <- item:
			return true
		case <-self.ctx.Done():
			return false
		}
	case timeout == 0:
		select {
		case self.outbound <- item:
			return true
		default:
			return false
		}
	default:
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case self.outbound <- item:
			return true
		case <-timer.C:
			return false
		case <-self.ctx.Done():
			return false
		}
	}
}

func (self *MessageRouteClient) AddReceiveCallback(receiveCallback connect.ReceiveFunction) func() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	id := self.nextCallback
	self.nextCallback += 1
	self.callbacks[id] = receiveCallback
	return func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		delete(self.callbacks, id)
	}
}

// OnSessionReplaced registers a callback for every session after the first: each one is a new
// connection at the server, so whatever said Hello on the old one must say it again.
func (self *MessageRouteClient) OnSessionReplaced(callback func()) func() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	id := self.nextCallback
	self.nextCallback += 1
	self.replaced[id] = callback
	return func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		delete(self.replaced, id)
	}
}

// Close ends the session and the tunnel. Frames still queued are given back, each ack told.
func (self *MessageRouteClient) Close() {
	self.closeOnce.Do(func() {
		self.cancel()
		self.wg.Wait()
		for {
			select {
			case item := <-self.outbound:
				connect.MessagePoolReturn(item.frame.MessageBytes)
				if item.ack != nil {
					item.ack(errMessageRouteClosed)
				}
				continue
			default:
			}
			break
		}
		if self.tunnel != nil {
			self.tunnel.Close()
		}
	})
}
