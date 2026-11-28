// Package hotspot shares a device's URnetwork tunnel with the devices on
// its hotspot or tethering, such as a laptop on the phone's wi-fi hotspot or
// on usb or bluetooth tethering, through an HTTP and a SOCKS5 proxy on the
// phone. The proxy servers are the proxy repo's (github.com/urnetwork/proxy),
// the same HTTP and SOCKS components the hosted proxies run; this package adds
// where they listen, the credentials and the tunnel-only dial. It is a child
// of the sdk that only the android binding carries (sdk/build `build_android`),
// so no other build of the sdk links the proxy repo.
//
// Safety, in the order a request meets it:
//   - Off until the owner turns it on, which is the space's stored choice
//     (sdk.HotspotProxyPreferences). Nothing listens while it is off.
//   - Listens only on the private IPv4 address of an interface the platform
//     reports as a tethering downstream and not as one of the device's own
//     networks: never the wildcard, loopback, or a cellular, wi-fi, ethernet or
//     vpn network's address (hotspotProxyBindAddresses).
//   - Admits a connection only from that address's subnet, and at most
//     maxConnectionCount at once (hotspotProxyScreeningListener).
//   - Requires the generated user name and password on every request. A
//     request without them is refused before anything is dialed.
//   - Dials only through the tunnel (sdk.DeviceLocal.DialTunnelOnlyContext):
//     a device that is not connected refuses the request, and nothing ever
//     falls back to the phone's own network.
//
// Not on ios: ios suspends an app shortly after it leaves the foreground,
// which closes its listeners, and the packet tunnel extension that keeps
// running has too tight a memory limit to serve other devices' connections.
//
// A HotspotProxy is safe for concurrent use. Its status listeners are called
// on the goroutine of the change, outside its locks.
package hotspot

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/proxy"
	"github.com/urnetwork/sdk"
)

// The status error ids. Empty is no error.
const (
	// on, and there is no hotspot or tethering interface to listen on
	HotspotProxyErrorNoHotspot = "hotspot_proxy_error_no_hotspot"
	// on, and a hotspot address could not be listened on, e.g. its ports are
	// taken by another app
	HotspotProxyErrorListenFailed = "hotspot_proxy_error_listen_failed"
	// the proxy closed with its device; the next device has its own proxy
	HotspotProxyErrorClosed = "hotspot_proxy_error_closed"
)

// Tunables of a HotspotProxy.
type hotspotProxySettings struct {
	// The HTTP proxy's port on each hotspot address: the usual HTTP proxy port,
	// fixed so a shared device's settings stay valid from one session to the
	// next. Zero picks a free port.
	httpPort int
	// The SOCKS5 proxy's port on each hotspot address: the SOCKS port. Zero
	// picks a free port.
	socksPort int
	// The most connections served at once over every listener. Each holds a
	// tunnel connection and relay buffers on the phone. A connection over the
	// cap is closed as it is accepted. Non-positive is no cap.
	maxConnectionCount int
	// The random characters of a generated user name, after its prefix.
	usernameLength int
	// The characters of a generated password: 20 of 62 is about 119 bits.
	passwordLength int
	// Binds one listener at exactly the address. Tests bind a loopback
	// listener in its place.
	listen func(ctx context.Context, address netip.AddrPort) (net.Listener, error)
	// How long a relayed connection may sit idle before it is closed, for
	// the http and the socks proxy. Longer than the hosted proxies' 30 s: a
	// shared laptop keeps idle sessions (chat, ssh) whose heartbeats are
	// further apart, and maxConnectionCount bounds what idle ones hold.
	proxyReadTimeout time.Duration
	// The proxy repo's server settings. proxyReadTimeout replaces their read
	// timeout.
	httpProxySettings  *proxy.HttpProxySettings
	socksProxySettings *proxy.SocksProxySettings
}

func defaultHotspotProxySettings() *hotspotProxySettings {
	return &hotspotProxySettings{
		httpPort:           8080,
		socksPort:          1080,
		maxConnectionCount: 256,
		usernameLength:     8,
		passwordLength:     20,
		listen:             listenTcp4,
		proxyReadTimeout:   5 * time.Minute,
		httpProxySettings:  proxy.DefaultHttpProxySettings(),
		socksProxySettings: proxy.DefaultSocksProxySettings(),
	}
}

// Dials through the tunnel only.
type hotspotProxyDialFunc func(ctx context.Context, network string, address string) (net.Conn, error)

// Where the proxy keeps the owner's choice and the credentials.
type hotspotProxyStore interface {
	GetHotspotProxyPreferences() *sdk.HotspotProxyPreferences
	SetHotspotProxyPreferences(preferences *sdk.HotspotProxyPreferences) error
}

// The store of a device without local state: the choice lasts for the device.
type memoryHotspotProxyStore struct {
	stateLock   sync.Mutex
	preferences sdk.HotspotProxyPreferences
}

func (self *memoryHotspotProxyStore) GetHotspotProxyPreferences() *sdk.HotspotProxyPreferences {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	preferences := self.preferences
	return &preferences
}

func (self *memoryHotspotProxyStore) SetHotspotProxyPreferences(preferences *sdk.HotspotProxyPreferences) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.preferences = *preferences
	return nil
}

// Called when the status changes. Read the status with GetStatus.
type HotspotProxyStatusChangeListener interface {
	HotspotProxyStatusChanged()
}

// One hotspot address the proxy listens on, with what a shared device needs
// to use it.
type HotspotProxyEndpoint struct {
	// the hotspot interface, e.g. wlan1
	InterfaceName string
	// the address on that interface, e.g. 192.168.43.1
	Host      string
	HttpPort  int
	SocksPort int
	// the HTTP proxy with its credentials, http://user:password@host:port
	HttpProxyUrl string
	// the SOCKS5 proxy with its credentials, resolving names through the
	// tunnel, socks5h://user:password@host:port
	SocksProxyUrl string
}

// A list of endpoints, since gomobile binds no slice of structs.
type HotspotProxyEndpointList struct {
	endpoints []*HotspotProxyEndpoint
}

func (self *HotspotProxyEndpointList) Len() int {
	return len(self.endpoints)
}

func (self *HotspotProxyEndpointList) Get(i int) *HotspotProxyEndpoint {
	return self.endpoints[i]
}

// What the proxy is doing. Each GetStatus is a new copy.
type HotspotProxyStatus struct {
	// the owner turned the proxy on
	Enabled bool
	// listening on at least one hotspot address
	Listening bool
	// one of the HotspotProxyError ids, empty for none
	ErrorId string
	// the credentials, kept while the proxy is off; empty before the first
	// time it is turned on
	Username string
	Password string
	// the addresses listened on; empty while not listening
	Endpoints *HotspotProxyEndpointList
}

// The listeners of one hotspot address, served until stop.
type hotspotProxyBinding struct {
	bindAddress   hotspotProxyBindAddress
	cancel        context.CancelFunc
	httpAddrPort  netip.AddrPort
	socksAddrPort netip.AddrPort
	// a serve loop ended while its context was live
	failed  atomic.Bool
	workers sync.WaitGroup
}

// Ends the listeners and every connection they serve, and waits for them.
func (self *hotspotProxyBinding) stop() {
	self.cancel()
	self.workers.Wait()
}

type HotspotProxy struct {
	ctx    context.Context
	cancel context.CancelFunc

	settings *hotspotProxySettings
	log      connect.Logger
	dial     hotspotProxyDialFunc
	store    hotspotProxyStore

	httpProxy   *proxy.HttpProxy
	socksProxy  *proxy.SocksProxy
	connections *hotspotProxyConnectionBudget
	// connections the screen closed
	refusedCount atomic.Int64

	// Serializes every change: the store write, then the listeners. bindings
	// change only under it. A listen and a binding's teardown run under it,
	// never under stateLock.
	operationLock sync.Mutex
	bindings      map[hotspotProxyBindAddress]*hotspotProxyBinding

	stateLock   sync.Mutex
	preferences sdk.HotspotProxyPreferences
	interfaces  []hotspotProxyInterface
	status      *HotspotProxyStatus
	closed      bool

	statusChangeListeners *connect.CallbackList[HotspotProxyStatusChangeListener]

	runDone chan struct{}
}

// The hotspot proxy of a device, which lives as long as the device: closing
// the device closes its listeners and connections. Make one for each device.
// It reads the owner's choice from the device's local state and does not
// listen until it is on and the platform reports a hotspot (SetInterfaces).
func NewHotspotProxyWithDefaults(device *sdk.DeviceLocal) *HotspotProxy {
	if device == nil {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return newHotspotProxy(ctx, defaultHotspotProxySettings(), notConnectedDial, &memoryHotspotProxyStore{}, connect.DefaultLogger())
	}
	var store hotspotProxyStore = &memoryHotspotProxyStore{}
	if networkSpace := device.GetNetworkSpace(); networkSpace != nil {
		if asyncLocalState := networkSpace.GetAsyncLocalState(); asyncLocalState != nil {
			if localState := asyncLocalState.GetLocalState(); localState != nil {
				store = localState
			}
		}
	}
	return newHotspotProxy(device.Ctx(), defaultHotspotProxySettings(), device.DialTunnelOnlyContext, store, connect.DefaultLogger())
}

func notConnectedDial(ctx context.Context, network string, address string) (net.Conn, error) {
	return nil, sdk.ErrTunnelNotConnected
}

// Starts the proxy, closed with ctx. Nothing listens until the stored choice
// is on and SetInterfaces reports a hotspot.
func newHotspotProxy(
	ctx context.Context,
	settings *hotspotProxySettings,
	dial hotspotProxyDialFunc,
	store hotspotProxyStore,
	log connect.Logger,
) *HotspotProxy {
	cancelCtx, cancel := context.WithCancel(ctx)
	self := &HotspotProxy{
		ctx:                   cancelCtx,
		cancel:                cancel,
		settings:              settings,
		log:                   log,
		dial:                  dial,
		store:                 store,
		connections:           newHotspotProxyConnectionBudget(settings.maxConnectionCount),
		bindings:              map[hotspotProxyBindAddress]*hotspotProxyBinding{},
		statusChangeListeners: connect.NewCallbackList[HotspotProxyStatusChangeListener](),
		runDone:               make(chan struct{}),
	}
	if preferences := store.GetHotspotProxyPreferences(); preferences != nil {
		self.preferences = *preferences
	}

	httpSettings := *settings.httpProxySettings
	httpSettings.Log = log
	httpSettings.ProxyReadTimeout = settings.proxyReadTimeout
	self.httpProxy = proxy.NewHttpProxy(&httpSettings)
	self.httpProxy.ConnectDialContextWithRequest = self.dialHttp

	socksSettings := *settings.socksProxySettings
	socksSettings.Log = log
	socksSettings.ProxyReadTimeout = settings.proxyReadTimeout
	self.socksProxy = proxy.NewSocksProxy(&socksSettings)
	self.socksProxy.ValidUser = self.validSocksUser
	self.socksProxy.ConnectDialWithRequest = self.dialSocks

	self.publishStatus()
	go connect.HandleError(self.run)
	return self
}

// Closes every listener when the device or Close ends the context.
func (self *HotspotProxy) run() {
	defer close(self.runDone)
	<-self.ctx.Done()
	func() {
		self.operationLock.Lock()
		defer self.operationLock.Unlock()
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.closed = true
		}()
		self.reconcileBindingsWithLock(nil, false)
		self.publishStatus()
	}()
	self.notifyStatusChanged()
}

// Turns the proxy on or off and stores the choice. The first time it is turned
// on it generates the credentials, which stay for later sessions. Off closes
// every listener and every connection.
func (self *HotspotProxy) SetEnabled(enabled bool) {
	changed := func() bool {
		self.operationLock.Lock()
		defer self.operationLock.Unlock()
		preferences, closed := func() (sdk.HotspotProxyPreferences, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			// a context that ended is closing, before run marks it closed
			return self.preferences, self.closed || self.ctx.Err() != nil
		}()
		if closed {
			return false
		}
		next := preferences
		next.Enabled = enabled
		if enabled && (next.Username == "" || next.Password == "") {
			username, password, err := newHotspotProxyCredentials(self.settings)
			if err != nil {
				// never on without credentials
				self.log.Warningf("[hotspot-proxy] credentials could not be generated, staying off: %v", err)
				return false
			}
			next.Username, next.Password = username, password
		}
		if next == preferences {
			return false
		}
		self.storePreferencesWithLock(next)
		self.reconcileWithLock(false)
		return true
	}()
	if changed {
		self.notifyStatusChanged()
	}
}

func (self *HotspotProxy) GetEnabled() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.preferences.Enabled
}

// Replaces the credentials with new ones. Every connection signed in with the
// old ones is closed: the listeners restart, and only the new credentials
// sign in.
func (self *HotspotProxy) RegenerateCredentials() {
	changed := func() bool {
		self.operationLock.Lock()
		defer self.operationLock.Unlock()
		preferences, closed := func() (sdk.HotspotProxyPreferences, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			// a context that ended is closing, before run marks it closed
			return self.preferences, self.closed || self.ctx.Err() != nil
		}()
		if closed {
			return false
		}
		username, password, err := newHotspotProxyCredentials(self.settings)
		if err != nil {
			self.log.Warningf("[hotspot-proxy] credentials could not be generated: %v", err)
			return false
		}
		next := preferences
		next.Username, next.Password = username, password
		self.storePreferencesWithLock(next)
		self.reconcileWithLock(true)
		return true
	}()
	if changed {
		self.notifyStatusChanged()
	}
}

// Takes the platform's report of its interfaces, all of them or only the
// tethering downstreams; the proxy chooses its addresses from it. Call on
// every change of the hotspot or tethering state.
func (self *HotspotProxy) SetInterfaces(interfaces *HotspotInterfaceList) {
	interfaceCopies := copyHotspotInterfaces(interfaces)
	changed := func() bool {
		self.operationLock.Lock()
		defer self.operationLock.Unlock()
		closed := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.interfaces = interfaceCopies
			return self.closed || self.ctx.Err() != nil
		}()
		if closed {
			return false
		}
		return self.reconcileWithLock(false)
	}()
	if changed {
		self.notifyStatusChanged()
	}
}

// A copy of the current status.
func (self *HotspotProxy) GetStatus() *HotspotProxyStatus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return copyHotspotProxyStatus(self.status)
}

func (self *HotspotProxy) AddStatusChangeListener(listener HotspotProxyStatusChangeListener) sdk.Sub {
	callbackId := self.statusChangeListeners.Add(listener)
	return &hotspotProxySub{
		close: func() {
			self.statusChangeListeners.Remove(callbackId)
		},
	}
}

// Closes every listener and connection and waits for them. The stored choice
// stays for the next device's proxy.
func (self *HotspotProxy) Close() {
	self.cancel()
	<-self.runDone
}

type hotspotProxySub struct {
	close func()
}

func (self *hotspotProxySub) Close() {
	self.close()
}

// Stores the choice and adopts it. A failed write is logged, and the choice
// still applies to this device. Called with operationLock.
func (self *HotspotProxy) storePreferencesWithLock(preferences sdk.HotspotProxyPreferences) {
	stored := preferences
	if err := self.store.SetHotspotProxyPreferences(&stored); err != nil {
		self.log.Warningf("[hotspot-proxy] the choice could not be stored: %v", err)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.preferences = preferences
}

// Moves the listeners to what the choice and the interfaces call for, then
// publishes the status. restart also restarts the listeners that stay, which
// closes their connections. Returns whether the status changed. Called with
// operationLock.
func (self *HotspotProxy) reconcileWithLock(restart bool) bool {
	bindAddresses := func() []hotspotProxyBindAddress {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed || !self.preferences.Enabled ||
			self.preferences.Username == "" || self.preferences.Password == "" {
			return nil
		}
		return hotspotProxyBindAddresses(self.interfaces)
	}()
	self.reconcileBindingsWithLock(bindAddresses, restart)
	return self.publishStatus()
}

// Stops the bindings that are not wanted or have failed, and starts the
// wanted ones that are missing. A failed listen is logged and shows in the
// status. Called with operationLock.
func (self *HotspotProxy) reconcileBindingsWithLock(bindAddresses []hotspotProxyBindAddress, restart bool) {
	wanted := map[hotspotProxyBindAddress]bool{}
	for _, bindAddress := range bindAddresses {
		wanted[bindAddress] = true
	}
	for bindAddress, binding := range self.bindings {
		if restart || !wanted[bindAddress] || binding.failed.Load() {
			delete(self.bindings, bindAddress)
			binding.stop()
			if self.log.V(1).Enabled() {
				self.log.Infof("[hotspot-proxy] stopped on %s %s", bindAddress.interfaceName, bindAddress.prefix.Addr())
			}
		}
	}
	for _, bindAddress := range bindAddresses {
		if _, ok := self.bindings[bindAddress]; ok {
			continue
		}
		binding, err := self.startBinding(bindAddress)
		if err != nil {
			self.log.Warningf("[hotspot-proxy] could not listen on %s %s: %v", bindAddress.interfaceName, bindAddress.prefix.Addr(), err)
			self.bindings[bindAddress] = &hotspotProxyBinding{
				bindAddress: bindAddress,
				cancel:      func() {},
			}
			self.bindings[bindAddress].failed.Store(true)
			continue
		}
		self.bindings[bindAddress] = binding
		self.log.Infof("[hotspot-proxy] listening on %s http=%s socks=%s", bindAddress.interfaceName, binding.httpAddrPort, binding.socksAddrPort)
	}
}

// Listens for http and socks on the address and serves both until the
// binding stops or the proxy closes.
func (self *HotspotProxy) startBinding(bindAddress hotspotProxyBindAddress) (*hotspotProxyBinding, error) {
	ctx, cancel := context.WithCancel(self.ctx)
	addr := bindAddress.prefix.Addr()
	httpListener, err := self.settings.listen(ctx, netip.AddrPortFrom(addr, uint16(self.settings.httpPort)))
	if err != nil {
		cancel()
		return nil, err
	}
	socksListener, err := self.settings.listen(ctx, netip.AddrPortFrom(addr, uint16(self.settings.socksPort)))
	if err != nil {
		httpListener.Close()
		cancel()
		return nil, err
	}
	binding := &hotspotProxyBinding{
		bindAddress:   bindAddress,
		cancel:        cancel,
		httpAddrPort:  listenerAddrPort(httpListener),
		socksAddrPort: listenerAddrPort(socksListener),
	}
	sourcePrefix := bindAddress.prefix.Masked()
	serve := func(listener net.Listener, serveListener func(ctx context.Context, l net.Listener) error) {
		screened := &hotspotProxyScreeningListener{
			Listener:     listener,
			sourcePrefix: sourcePrefix,
			budget:       self.connections,
			refusedCount: &self.refusedCount,
		}
		binding.workers.Add(1)
		go func() {
			defer binding.workers.Done()
			connect.HandleError(func() {
				err := serveListener(ctx, screened)
				if ctx.Err() == nil {
					// the listener failed under a live binding; the next
					// change replaces it
					binding.failed.Store(true)
					self.log.Warningf("[hotspot-proxy] stopped serving on %s: %v", bindAddress.interfaceName, err)
				}
			})
		}()
	}
	serve(httpListener, self.httpProxy.Serve)
	serve(socksListener, self.socksProxy.Serve)
	return binding, nil
}

func listenerAddrPort(listener net.Listener) netip.AddrPort {
	if tcpAddr, ok := listener.Addr().(*net.TCPAddr); ok {
		return tcpAddr.AddrPort()
	}
	return netip.AddrPort{}
}

// Rebuilds the status from the choice and the bindings. Returns whether it
// changed. Called with operationLock, or from the constructor.
func (self *HotspotProxy) publishStatus() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	status := &HotspotProxyStatus{
		Enabled:   self.preferences.Enabled,
		Username:  self.preferences.Username,
		Password:  self.preferences.Password,
		Endpoints: &HotspotProxyEndpointList{},
	}
	failed := false
	running := []*hotspotProxyBinding{}
	for _, binding := range self.bindings {
		if binding.failed.Load() {
			failed = true
		} else {
			running = append(running, binding)
		}
	}
	// by interface name, then address, as hotspotProxyBindAddresses orders them
	slices.SortFunc(running, func(a *hotspotProxyBinding, b *hotspotProxyBinding) int {
		if c := cmp.Compare(a.bindAddress.interfaceName, b.bindAddress.interfaceName); c != 0 {
			return c
		}
		return a.bindAddress.prefix.Addr().Compare(b.bindAddress.prefix.Addr())
	})
	for _, binding := range running {
		bindAddress := binding.bindAddress
		host := bindAddress.prefix.Addr().String()
		status.Endpoints.endpoints = append(status.Endpoints.endpoints, &HotspotProxyEndpoint{
			InterfaceName: bindAddress.interfaceName,
			Host:          host,
			HttpPort:      int(binding.httpAddrPort.Port()),
			SocksPort:     int(binding.socksAddrPort.Port()),
			HttpProxyUrl:  hotspotProxyUrl("http", self.preferences, host, binding.httpAddrPort.Port()),
			SocksProxyUrl: hotspotProxyUrl("socks5h", self.preferences, host, binding.socksAddrPort.Port()),
		})
	}
	status.Listening = 0 < len(status.Endpoints.endpoints)
	switch {
	case !status.Enabled:
	case self.closed:
		status.ErrorId = HotspotProxyErrorClosed
	case failed:
		status.ErrorId = HotspotProxyErrorListenFailed
	case !status.Listening:
		status.ErrorId = HotspotProxyErrorNoHotspot
	}
	changed := !hotspotProxyStatusEqual(self.status, status)
	self.status = status
	return changed
}

func hotspotProxyUrl(scheme string, preferences sdk.HotspotProxyPreferences, host string, port uint16) string {
	proxyUrl := &url.URL{
		Scheme: scheme,
		User:   url.UserPassword(preferences.Username, preferences.Password),
		Host:   net.JoinHostPort(host, strconv.Itoa(int(port))),
	}
	return proxyUrl.String()
}

func copyHotspotProxyStatus(status *HotspotProxyStatus) *HotspotProxyStatus {
	if status == nil {
		return &HotspotProxyStatus{Endpoints: &HotspotProxyEndpointList{}}
	}
	statusCopy := *status
	statusCopy.Endpoints = &HotspotProxyEndpointList{}
	for _, endpoint := range status.Endpoints.endpoints {
		endpointCopy := *endpoint
		statusCopy.Endpoints.endpoints = append(statusCopy.Endpoints.endpoints, &endpointCopy)
	}
	return &statusCopy
}

func hotspotProxyStatusEqual(a *HotspotProxyStatus, b *HotspotProxyStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Enabled != b.Enabled || a.Listening != b.Listening || a.ErrorId != b.ErrorId ||
		a.Username != b.Username || a.Password != b.Password ||
		len(a.Endpoints.endpoints) != len(b.Endpoints.endpoints) {
		return false
	}
	for i, endpoint := range a.Endpoints.endpoints {
		if *endpoint != *b.Endpoints.endpoints[i] {
			return false
		}
	}
	return true
}

func (self *HotspotProxy) notifyStatusChanged() {
	for _, listener := range self.statusChangeListeners.Get() {
		connect.HandleError(listener.HotspotProxyStatusChanged)
	}
}

// Whether the credentials are the proxy's.
func (self *HotspotProxy) credentialsMatch(username string, password string) bool {
	expectedUsername, expectedPassword := func() (string, string) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.preferences.Username, self.preferences.Password
	}()
	return hotspotProxyCredentialsMatch(username, password, expectedUsername, expectedPassword)
}

// The http proxy's dial: a request without the credentials is refused with
// 407, before anything is dialed, and a device that is not connected refuses
// with 502 rather than retrying.
func (self *HotspotProxy) dialHttp(ctx context.Context, r *http.Request, network string, address string) (net.Conn, error) {
	username, password, _ := httpProxyRequestCredentials(r)
	if !self.credentialsMatch(username, password) {
		return nil, proxy.NewConnectRefusal(proxy.ConnectRefusalUnauthenticated, errHotspotProxyUnauthenticated)
	}
	conn, err := self.dial(ctx, network, address)
	if errors.Is(err, sdk.ErrTunnelNotConnected) {
		return nil, proxy.NewConnectRefusal(proxy.ConnectRefusalReset, err)
	}
	return conn, err
}

// The socks proxy's credential check. The proxy offers only the user/password
// method, so a client without credentials is refused at negotiation.
func (self *HotspotProxy) validSocksUser(username string, password string, userAddr string) bool {
	return self.credentialsMatch(username, password)
}

// The socks proxy's dial, for CONNECT and every UDP ASSOCIATE flow. A device
// that is not connected answers network unreachable.
func (self *HotspotProxy) dialSocks(ctx context.Context, r proxy.SocksRequest, network string, address string) (net.Conn, error) {
	conn, err := self.dial(ctx, network, address)
	if errors.Is(err, sdk.ErrTunnelNotConnected) {
		return nil, fmt.Errorf("%w: %w", err, syscall.ENETUNREACH)
	}
	return conn, err
}
