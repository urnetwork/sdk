package sdk

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/urnetwork/connect/v2026"
)

// A single net.Conn is initiated by the DeviceRemote and carries two
// multiplexed bi-directional streams, framed as tagged messages and exposed
// as two separate net.Conns:
//
//	forward: DeviceRemote is the net/rpc client of DeviceLocalRpc
//	reverse: DeviceLocalRpc is the net/rpc client of DeviceRemoteRpc
//
// The framing mirrors connect/transport.go's pooled websocket usage
// (MessagePoolReadAll + WriteMessage + MessagePoolReturn): every websocket
// binary message is `[streamTag][payload...]`, where the leading byte selects
// the stream. The payload is the raw byte stream from net/rpc's gob codec.

const (
	deviceRpcStreamForward uint8 = 0
	deviceRpcStreamReverse uint8 = 1
	deviceRpcStreamCount         = 2
)

// DeviceRpcDialer establishes the single underlying connection initiated by the
// DeviceRemote and returns the two multiplexed bi-directional streams.
type DeviceRpcDialer interface {
	Dial(ctx context.Context) (forward net.Conn, reverse net.Conn, err error)
}

// DeviceRpcListener accepts the single underlying connection and returns the two
// multiplexed bi-directional streams with the same orientation as the dialer.
type DeviceRpcListener interface {
	Accept(ctx context.Context) (forward net.Conn, reverse net.Conn, err error)
	Close() error
}

// deviceRpcMux multiplexes deviceRpcStreamCount logical net.Conns over a single
// websocket connection.
type deviceRpcMux struct {
	ctx    context.Context
	cancel context.CancelFunc
	log    connect.Logger

	ws *websocket.Conn

	pingTimeout  time.Duration
	readTimeout  time.Duration
	writeTimeout time.Duration

	// pooled, tag-prefixed frames pending write. drained and returned to the
	// pool on teardown.
	send chan []byte

	conns [deviceRpcStreamCount]*deviceRpcMuxConn

	closeOnce sync.Once
}

func newDeviceRpcMux(ctx context.Context, ws *websocket.Conn, settings *deviceRpcSettings) *deviceRpcMux {
	cancelCtx, cancel := context.WithCancel(ctx)

	pingTimeout := settings.KeepAliveTimeout
	readTimeout := settings.KeepAliveTimeout * time.Duration(settings.KeepAliveRetryCount+1)

	mux := &deviceRpcMux{
		ctx:          cancelCtx,
		cancel:       cancel,
		log:          settings.logger(),
		ws:           ws,
		pingTimeout:  pingTimeout,
		readTimeout:  readTimeout,
		writeTimeout: settings.MuxWriteTimeout,
		send:         make(chan []byte, settings.MuxSendBufferSize),
	}
	for i := range mux.conns {
		mux.conns[i] = newDeviceRpcMuxConn(mux, uint8(i), settings)
	}

	if 0 < readTimeout {
		ws.SetReadDeadline(time.Now().Add(readTimeout))
		ws.SetPongHandler(func(string) error {
			ws.SetReadDeadline(time.Now().Add(readTimeout))
			return nil
		})
	}

	go connect.HandleError(mux.writeLoop, cancel)
	go connect.HandleError(mux.readLoop, cancel)
	return mux
}

func (self *deviceRpcMux) close() {
	self.closeOnce.Do(func() {
		self.log.Infof("[mux]close")
		self.cancel()
		self.ws.Close()
	})
}

func (self *deviceRpcMux) writeLoop() {
	defer func() {
		self.close()
		// drain any pooled frames still queued so they return to the pool
		for {
			select {
			case b := <-self.send:
				connect.MessagePoolReturn(b)
			default:
				return
			}
		}
	}()

	var ping <-chan time.Time
	if 0 < self.pingTimeout {
		ticker := time.NewTicker(self.pingTimeout)
		defer ticker.Stop()
		ping = ticker.C
	}

	for {
		select {
		case <-self.ctx.Done():
			return
		case b := <-self.send:
			if 0 < self.writeTimeout {
				self.ws.SetWriteDeadline(time.Now().Add(self.writeTimeout))
			}
			err := self.ws.WriteMessage(websocket.BinaryMessage, b)
			connect.MessagePoolReturn(b)
			if err != nil {
				self.log.Infof("[mux]write done = %s", err)
				return
			}
		case <-ping:
			// match the message-write path: only apply a deadline when one is
			// configured. with writeTimeout == 0 the zero time.Time means "no
			// deadline"; passing time.Now() would be an already-expired deadline
			// and every ping would fail immediately, tearing down the mux.
			var deadline time.Time
			if 0 < self.writeTimeout {
				deadline = time.Now().Add(self.writeTimeout)
				self.ws.SetWriteDeadline(deadline)
			}
			if err := self.ws.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
				self.log.Infof("[mux]ping done = %s", err)
				return
			}
		}
	}
}

func (self *deviceRpcMux) readLoop() {
	// readLoop is the sole producer for each conn's receive channel, so once
	// this defer runs no further frames can be pushed. drain whatever is still
	// buffered back to the pool (mirrors writeLoop draining self.send), so
	// pooled frames are not leaked when the mux is torn down.
	defer func() {
		self.close()
		for _, conn := range self.conns {
			conn.drainReceive()
		}
	}()

	for {
		messageType, r, err := self.ws.NextReader()
		if err != nil {
			self.log.Infof("[mux]read done = %s", err)
			return
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		message, err := connect.MessagePoolReadAll(r)
		if err != nil {
			self.log.Infof("[mux]read all done = %s", err)
			return
		}
		if 0 < self.readTimeout {
			self.ws.SetReadDeadline(time.Now().Add(self.readTimeout))
		}
		if len(message) < 1 {
			connect.MessagePoolReturn(message)
			continue
		}
		tag := message[0]
		if int(tag) >= deviceRpcStreamCount {
			connect.MessagePoolReturn(message)
			continue
		}
		// ownership of message passes to the conn, which returns it to the pool
		// once fully consumed by Read
		if !self.conns[tag].pushReceive(message) {
			connect.MessagePoolReturn(message)
			return
		}
	}
}

// compile check that deviceRpcMuxConn conforms to net.Conn
var _ net.Conn = (*deviceRpcMuxConn)(nil)

type deviceRpcMuxConn struct {
	mux *deviceRpcMux
	tag uint8

	receive chan []byte

	readMu  sync.Mutex
	readMsg []byte // current pooled frame, nil when none
	readOff int    // next unread index into readMsg (starts at 1, past the tag)
}

func newDeviceRpcMuxConn(mux *deviceRpcMux, tag uint8, settings *deviceRpcSettings) *deviceRpcMuxConn {
	return &deviceRpcMuxConn{
		mux:     mux,
		tag:     tag,
		receive: make(chan []byte, settings.MuxReceiveBufferSize),
	}
}

func (self *deviceRpcMuxConn) pushReceive(message []byte) bool {
	select {
	case <-self.mux.ctx.Done():
		return false
	case self.receive <- message:
		return true
	}
}

// drainReceive returns any buffered pooled frames to the pool. Only safe to
// call once readLoop (the sole producer) has stopped pushing.
func (self *deviceRpcMuxConn) drainReceive() {
	for {
		select {
		case message := <-self.receive:
			connect.MessagePoolReturn(message)
		default:
			return
		}
	}
}

func (self *deviceRpcMuxConn) Read(p []byte) (int, error) {
	self.readMu.Lock()
	defer self.readMu.Unlock()

	for self.readMsg == nil || self.readOff >= len(self.readMsg) {
		if self.readMsg != nil {
			connect.MessagePoolReturn(self.readMsg)
			self.readMsg = nil
		}
		select {
		case <-self.mux.ctx.Done():
			return 0, io.EOF
		case message := <-self.receive:
			self.readMsg = message
			self.readOff = 1 // skip the tag byte
		}
	}

	n := copy(p, self.readMsg[self.readOff:])
	self.readOff += n
	if self.readOff >= len(self.readMsg) {
		connect.MessagePoolReturn(self.readMsg)
		self.readMsg = nil
	}
	return n, nil
}

func (self *deviceRpcMuxConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b := connect.MessagePoolGet(1 + len(p))
	b[0] = self.tag
	copy(b[1:], p)
	select {
	case <-self.mux.ctx.Done():
		connect.MessagePoolReturn(b)
		return 0, io.ErrClosedPipe
	case self.mux.send <- b:
		return len(p), nil
	}
}

// Close tears down the whole mux. The two streams share one connection, so the
// rpc session ending on either stream ends both, matching the prior behavior
// where closing the rpc client closed the connection.
func (self *deviceRpcMuxConn) Close() error {
	self.mux.close()
	return nil
}

func (self *deviceRpcMuxConn) LocalAddr() net.Addr  { return self.mux.ws.LocalAddr() }
func (self *deviceRpcMuxConn) RemoteAddr() net.Addr { return self.mux.ws.RemoteAddr() }

// net/rpc does not use deadlines; liveness is handled at the websocket layer.
func (self *deviceRpcMuxConn) SetDeadline(t time.Time) error      { return nil }
func (self *deviceRpcMuxConn) SetReadDeadline(t time.Time) error  { return nil }
func (self *deviceRpcMuxConn) SetWriteDeadline(t time.Time) error { return nil }

// DeviceRpcKeyMaterial is the self-signed material for a single rpc session.
// The PEM values are Go-generated strings handed to the app verbatim. The app
// (and the network extension) must treat them as opaque and pass them back
// unchanged — all encoding/decoding lives here in the SDK.
type DeviceRpcKeyMaterial struct {
	serverPem     string // server certificate + private key
	serverCertPem string // server certificate (public)
	clientPem     string // client certificate + private key
	clientCertPem string // client certificate (public)
}

// GetServerPem returns the server certificate plus private key the listener
// presents; pass to DeviceLocal.SetRpcServer (or NewWebsocketDeviceRpcListener).
func (self *DeviceRpcKeyMaterial) GetServerPem() string {
	return self.serverPem
}

// GetServerCertPem returns the server certificate (public) the dialer pins;
// pass to DeviceRemote.SetRpcServer (or NewWebsocketDeviceRpcDialer).
func (self *DeviceRpcKeyMaterial) GetServerCertPem() string {
	return self.serverCertPem
}

// GetClientPem returns the client certificate plus private key the dialer
// presents (mTLS); pass to DeviceRemote.SetRpcServer (or
// NewWebsocketDeviceRpcDialer). The client private key must stay on the client
// and never be transferred to the server side.
func (self *DeviceRpcKeyMaterial) GetClientPem() string {
	return self.clientPem
}

// GetClientCertPem returns the client certificate (public) the listener pins;
// pass to DeviceLocal.SetRpcServer (or NewWebsocketDeviceRpcListener). This is
// the only client value safe to transfer to the server side.
func (self *DeviceRpcKeyMaterial) GetClientCertPem() string {
	return self.clientCertPem
}

// GenerateDeviceRpcKeyMaterial creates fresh self-signed server and client
// keypairs for a single rpc session (mTLS). Apps generate this per vpn session
// so each session uses unique key material. The return is a struct (rather than
// multiple values) so it can be bound to gomobile.
func GenerateDeviceRpcKeyMaterial() (*DeviceRpcKeyMaterial, error) {
	serverCertPem, serverKeyPem, err := generateSelfSignedCert()
	if err != nil {
		return nil, err
	}
	clientCertPem, clientKeyPem, err := generateSelfSignedCert()
	if err != nil {
		return nil, err
	}
	return &DeviceRpcKeyMaterial{
		serverPem:     string(serverCertPem) + string(serverKeyPem),
		serverCertPem: string(serverCertPem),
		clientPem:     string(clientCertPem) + string(clientKeyPem),
		clientCertPem: string(clientCertPem),
	}, nil
}

func generateSelfSignedCert() (certPem []byte, keyPem []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, nil, err
	}

	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject:      pkix.Name{CommonName: "urnetwork device rpc"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		// loopback SANs keep the certificate well-formed; pinning matches the
		// exact certificate, not the names
		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}

	certDer, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, err
	}
	keyDer, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}

	certPem = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDer})
	keyPem = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDer})
	return certPem, keyPem, nil
}

// pinVerifier returns a tls VerifyPeerCertificate that requires the peer's leaf
// certificate to exactly match pinned.
func pinVerifier(pinned *x509.Certificate) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("no peer certificate presented")
		}
		if !bytes.Equal(rawCerts[0], pinned.Raw) {
			return fmt.Errorf("peer certificate does not match pin")
		}
		return nil
	}
}

func parsePinnedCert(certPem []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPem)
	if block == nil {
		return nil, fmt.Errorf("invalid certificate PEM")
	}
	return x509.ParseCertificate(block.Bytes)
}

// clientTlsConfig builds a client tls.Config that pins the server certificate in
// serverCertPem and, when clientPem is non-empty, presents it as the client
// identity (mTLS).
func clientTlsConfig(log connect.Logger, serverCertPem string, clientPem string) (*tls.Config, error) {
	pinned, err := parsePinnedCert([]byte(serverCertPem))
	if err != nil {
		log.Infof("[dr]client tls: server cert pin parse err (serverCertPem len=%d) = %s", len(serverCertPem), err)
		return nil, err
	}
	config := &tls.Config{
		// server pinning is enforced by VerifyPeerCertificate
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: pinVerifier(pinned),
		MinVersion:            tls.VersionTLS12,
	}
	if len(clientPem) != 0 {
		cert, err := tls.X509KeyPair([]byte(clientPem), []byte(clientPem))
		if err != nil {
			log.Infof("[dr]client tls: client keypair err (clientPem len=%d) = %s", len(clientPem), err)
			return nil, err
		}
		config.Certificates = []tls.Certificate{cert}
	}
	return config, nil
}

// serverTlsConfig builds a server tls.Config presenting serverPem and, when
// clientCertPem is non-empty, requiring and pinning that client certificate (mTLS).
func serverTlsConfig(log connect.Logger, serverPem string, clientCertPem string) (*tls.Config, error) {
	cert, err := tls.X509KeyPair([]byte(serverPem), []byte(serverPem))
	if err != nil {
		log.Infof("[dlrpc]server tls: server keypair err (serverPem len=%d) = %s", len(serverPem), err)
		return nil, err
	}
	config := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if len(clientCertPem) != 0 {
		pinned, err := parsePinnedCert([]byte(clientCertPem))
		if err != nil {
			log.Infof("[dlrpc]server tls: client cert pin parse err (clientCertPem len=%d) = %s", len(clientCertPem), err)
			return nil, err
		}
		// require a client cert and pin it exactly via VerifyPeerCertificate
		config.ClientAuth = tls.RequireAnyClientCert
		config.VerifyPeerCertificate = pinVerifier(pinned)
	}
	return config, nil
}

// deviceRpcKeepAliveConfig enables OS-level TCP keepalive on the underlying
// connection so a dead peer is reaped by the kernel even when the websocket
// ping/pong layer is disabled (KeepAliveTimeout == 0). When the keepalive
// settings are positive the probe cadence matches the prior raw-TCP transport;
// otherwise the connection falls back to the OS default keepalive parameters.
func deviceRpcKeepAliveConfig(settings *deviceRpcSettings) net.KeepAliveConfig {
	config := net.KeepAliveConfig{Enable: true}
	if 0 < settings.KeepAliveTimeout && 0 < settings.KeepAliveRetryCount {
		interval := settings.KeepAliveTimeout / time.Duration(2*settings.KeepAliveRetryCount)
		config.Idle = interval
		config.Interval = interval
		config.Count = settings.KeepAliveRetryCount
	}
	return config
}

// compile check that WebsocketDeviceRpcDialer conforms to DeviceRpcDialer
var _ DeviceRpcDialer = (*WebsocketDeviceRpcDialer)(nil)

type WebsocketDeviceRpcDialer struct {
	address       *DeviceRemoteAddress
	clientPem     string // client identity to present (cert+key); optional
	serverCertPem string // server cert to pin; empty => unencrypted
	settings      *deviceRpcSettings
	log           connect.Logger
}

// NewWebsocketDeviceRpcDialer dials the device local at address. If
// serverCertPem is empty the connection is unencrypted (ws://); otherwise it is
// wss:// and the server must present exactly serverCertPem. When clientPem is
// non-empty it is presented as the client identity (mTLS). These are the
// GetClientPem / GetServerCertPem values from GenerateDeviceRpcKeyMaterial.
func NewWebsocketDeviceRpcDialer(address *DeviceRemoteAddress, clientPem string, serverCertPem string, settings *deviceRpcSettings) *WebsocketDeviceRpcDialer {
	return &WebsocketDeviceRpcDialer{
		address:       address,
		clientPem:     clientPem,
		serverCertPem: serverCertPem,
		settings:      settings,
		log:           settings.logger(),
	}
}

func (self *WebsocketDeviceRpcDialer) Dial(ctx context.Context) (net.Conn, net.Conn, error) {
	// dial the raw TCP conn with OS-level keepalive enabled; gorilla wraps this
	// conn with TLS for wss, so keepalive persists under encryption.
	netDialer := &net.Dialer{
		Timeout:         self.settings.RpcConnectTimeout,
		KeepAliveConfig: deviceRpcKeepAliveConfig(self.settings),
	}
	dialer := &websocket.Dialer{
		HandshakeTimeout: self.settings.RpcConnectTimeout,
		NetDialContext:   netDialer.DialContext,
	}
	scheme := "ws"
	if len(self.serverCertPem) != 0 {
		tlsConfig, err := clientTlsConfig(self.log, self.serverCertPem, self.clientPem)
		if err != nil {
			return nil, nil, err
		}
		dialer.TLSClientConfig = tlsConfig
		scheme = "wss"
	}
	u := url.URL{Scheme: scheme, Host: self.address.HostPort(), Path: "/"}

	self.log.Infof("[dr]dial %s (mtls=%t)", u.String(), len(self.clientPem) != 0)
	ws, _, err := dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		self.log.Infof("[dr]dial %s err = %s", u.String(), err)
		return nil, nil, err
	}
	self.log.Infof("[dr]dial %s connected", u.String())
	mux := newDeviceRpcMux(ctx, ws, self.settings)
	return mux.conns[deviceRpcStreamForward], mux.conns[deviceRpcStreamReverse], nil
}

// compile check that WebsocketDeviceRpcListener conforms to DeviceRpcListener
var _ DeviceRpcListener = (*WebsocketDeviceRpcListener)(nil)

type WebsocketDeviceRpcListener struct {
	ctx    context.Context
	cancel context.CancelFunc

	address       *DeviceRemoteAddress
	serverPem     string // server identity to present (cert+key); empty => unencrypted
	clientCertPem string // client cert to require + pin (mTLS); optional
	settings      *deviceRpcSettings
	log           connect.Logger

	upgrader websocket.Upgrader

	stateLock  sync.Mutex
	started    bool
	closed     bool
	httpServer *http.Server

	accepts  chan *deviceRpcMux
	serveErr chan error
}

// NewWebsocketDeviceRpcListener listens for device remotes at address. If
// serverPem is empty the server is unencrypted (http/ws://); otherwise it serves
// wss:// presenting the certificate+key in serverPem. When clientCertPem is
// non-empty the server requires and pins that client certificate (mTLS). These
// are the GetServerPem / GetClientCertPem values from GenerateDeviceRpcKeyMaterial.
func NewWebsocketDeviceRpcListener(address *DeviceRemoteAddress, serverPem string, clientCertPem string, settings *deviceRpcSettings) *WebsocketDeviceRpcListener {
	ctx, cancel := context.WithCancel(context.Background())
	return &WebsocketDeviceRpcListener{
		ctx:           ctx,
		cancel:        cancel,
		log:           settings.logger(),
		address:       address,
		serverPem:     serverPem,
		clientCertPem: clientCertPem,
		settings:      settings,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(*http.Request) bool { return true },
		},
		accepts:  make(chan *deviceRpcMux),
		serveErr: make(chan error, 1),
	}
}

func (self *WebsocketDeviceRpcListener) ensureStarted() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.closed {
		return fmt.Errorf("listener closed")
	}
	if self.started {
		return nil
	}

	// drop any serve error left by a previous listener generation so a fresh
	// listen does not immediately resurface a stale failure.
	select {
	case <-self.serveErr:
	default:
	}

	// listen with OS-level keepalive enabled on accepted connections so a dead
	// peer is reaped by the kernel even when the websocket ping layer is off.
	listenConfig := &net.ListenConfig{
		KeepAliveConfig: deviceRpcKeepAliveConfig(self.settings),
	}
	netListener, err := listenConfig.Listen(self.ctx, "tcp", self.address.HostPort())
	if err != nil {
		self.log.Infof("[dlrpc]listen %s err = %s", self.address.HostPort(), err)
		return err
	}

	serveMux := http.NewServeMux()
	serveMux.HandleFunc("/", self.handle)
	self.httpServer = &http.Server{
		Handler: serveMux,
		// handshake/connection failures on this localhost listener are not
		// actionable and a misconfigured client should not spam logs
		ErrorLog: log.New(io.Discard, "", 0),
	}

	useTls := len(self.serverPem) != 0
	if useTls {
		tlsConfig, err := serverTlsConfig(self.log, self.serverPem, self.clientCertPem)
		if err != nil {
			netListener.Close()
			return err
		}
		self.httpServer.TLSConfig = tlsConfig
	}

	self.log.Infof("[dlrpc]listen %s (tls=%t mtls=%t)", self.address.HostPort(), useTls, len(self.clientCertPem) != 0)
	self.started = true
	// capture the server locally: after a serve error we clear self.httpServer
	// so a later generation can rebind, and this goroutine must not observe that
	// reassignment.
	httpServer := self.httpServer
	go func() {
		var serveErr error
		if useTls {
			serveErr = httpServer.ServeTLS(netListener, "", "")
		} else {
			serveErr = httpServer.Serve(netListener)
		}
		// http.Server.Serve has already closed netListener on return. unless the
		// listener was closed intentionally, allow the next Accept to rebind a
		// fresh listener instead of wedging on the dead one.
		self.stateLock.Lock()
		if !self.closed {
			self.started = false
			self.httpServer = nil
		}
		self.stateLock.Unlock()
		select {
		case self.serveErr <- serveErr:
		default:
		}
	}()
	return nil
}

func (self *WebsocketDeviceRpcListener) handle(w http.ResponseWriter, r *http.Request) {
	ws, err := self.upgrader.Upgrade(w, r, nil)
	if err != nil {
		self.log.Infof("[dlrpc]ws upgrade err = %s", err)
		return
	}
	self.log.Infof("[dlrpc]accept from %s", ws.RemoteAddr())
	// the mux owns the websocket; it lives until torn down by the accepting
	// DeviceLocalRpc or by the listener being closed.
	mux := newDeviceRpcMux(self.ctx, ws, self.settings)
	select {
	case self.accepts <- mux:
	case <-self.ctx.Done():
		mux.close()
	}
}

func (self *WebsocketDeviceRpcListener) Accept(ctx context.Context) (net.Conn, net.Conn, error) {
	if err := self.ensureStarted(); err != nil {
		return nil, nil, err
	}
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-self.ctx.Done():
		return nil, nil, fmt.Errorf("listener closed")
	case err := <-self.serveErr:
		return nil, nil, err
	case mux := <-self.accepts:
		return mux.conns[deviceRpcStreamForward], mux.conns[deviceRpcStreamReverse], nil
	}
}

func (self *WebsocketDeviceRpcListener) Close() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.closed {
		return nil
	}
	self.closed = true
	self.cancel()
	if self.httpServer != nil {
		return self.httpServer.Close()
	}
	return nil
}
