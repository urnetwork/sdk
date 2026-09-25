// proxy device is not intended to use on gomobile platforms
//
//gomobile:noexport
package sdk

import (
	"context"
	"time"

	"github.com/urnetwork/connect"
)

// a proxy device is a device that is used through a proxy protocol - http or socks
// the device can be controlled through a normal `Device` interface;
// however, packet transport must use one of the proxy protocols
// "cloud proxy" is the typical use case of a proxy device, where the the packet routing is hosted on the platform
// ** note that with a cloud proxy, local packet routing is almost always disabled,
//    and a destination must be set in the device to route packets

// occasionally, due to extended network disconnect or the platform needing to move the resident,
// a new device will need to be created
// this callback should either:
// - configure the new device state and return `true`
// - or return `false` to cancel the proxy device
// if the callback is not set, the default behavior is `true`
type SetupNewDeviceCallback interface {
	// return false to cancel the proxy device
	SetupNewDevice(device Device, proxyConfigResult *ProxyConfigResult) bool
}

func DefaultProxyConfig() *ProxyConfig {
	return &ProxyConfig{
		EnableHttp: true,
	}
}

func DefaultProxyDeviceSettings() *ProxyDeviceSettings {
	return &ProxyDeviceSettings{
		ApiUrl:      "api.bringyour.com",
		PlatformUrl: "connect.bringyour.com",
	}
}

// `model.ProxyConfig`: the proxy device request on /network/auth-client
type ProxyConfig struct {
	LockCallerIp bool     `json:"lock_caller_ip"`
	LockIpList   []string `json:"lock_ip_list"`

	// EnableSocks, EnableHttp and HttpRequireAuth are not read by the
	// server: the SOCKS and WireGuard legs are gated by the network's plan
	// (pro.yml features), and HTTP is always on. They are kept for the wasm
	// projection (js/main.go) that still sets them.
	EnableSocks     bool `json:"enable_socks"`
	EnableHttp      bool `json:"enable_http"`
	HttpRequireAuth bool `json:"http_require_auth"`
	// HttpsRequireAuth asks for proxy authentication on the HTTPS leg
	HttpsRequireAuth bool `json:"https_require_auth"`
	// EnableWg asks for a WireGuard leg (issued only when the plan allows it)
	EnableWg bool `json:"enable_wg"`

	InitialDeviceState *ProxyDeviceState `json:"initial_device_state"`
}

// `model.ExtendedProxyDeviceState`. The server's
// `dns_resolver_settings` (connect.DnsResolverSettings) is not modeled here.
type ProxyDeviceState struct {
	Location           *ConnectLocation    `json:"location"`
	PerformanceProfile *PerformanceProfile `json:"performance_profile"`
	// CountryCode is the lowercase iso 3166-1 alpha-2 country the proxy
	// device should appear in
	CountryCode string `json:"country_code,omitempty"`
}

// `model.ProxyConfigResult`: `keepalive_seconds` plus the fields of
// `model.ProxyClient` flattened beside it (compare ProxyClient).
// ExpirationTime, HttpProxyAuth and SocksProxyAuth are never sent by the
// server; they are kept for the wasm projection (js/main.go).
type ProxyConfigResult struct {
	ExpirationTime   time.Time `json:"expiration_time"`
	KeepaliveSeconds int       `json:"keepalive_seconds"`
	ChangeId         int64     `json:"change_id,omitempty"`
	CreateTime       *Time     `json:"create_time,omitempty"`
	ProxyId          *Id       `json:"proxy_id,omitempty"`
	ClientId         *Id       `json:"client_id,omitempty"`
	InstanceId       *Id       `json:"instance_id,omitempty"`
	HttpProxyUrl     string    `json:"http_proxy_url,omitempty"`
	HttpsProxyUrl    string    `json:"https_proxy_url,omitempty"`
	SocksProxyUrl    string    `json:"socks_proxy_url,omitempty"`
	// ApiBaseUrl and ApiPort address the proxy's device api
	ApiBaseUrl string `json:"api_base_url,omitempty"`
	ProxyHost  string `json:"proxy_host,omitempty"`
	Block      string `json:"block,omitempty"`
	// the wire name is `socks_proxy_port` (an earlier tag, `sock_proxy_port`,
	// never matched the server)
	SocksProxyPort int    `json:"socks_proxy_port,omitempty"`
	HttpProxyPort  int    `json:"http_proxy_port,omitempty"`
	HttpsProxyPort int    `json:"https_proxy_port,omitempty"`
	ApiPort        int    `json:"api_port,omitempty"`
	AuthToken      string `json:"auth_token,omitempty"`
	// WgConfig is the WireGuard leg, nil when the plan does not include it
	WgConfig *WgConfig `json:"wg_config,omitempty"`

	HttpProxyAuth  *ProxyAuthResult `json:"http_proxy_auth"`
	SocksProxyAuth *ProxyAuthResult `json:"socks_proxy_auth"`
}

type ProxyAuthResult struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type ProxyDeviceSettings struct {
	ApiUrl      string
	PlatformUrl string
}

type ProxyDevice struct {
	ctx    context.Context
	cancel context.CancelFunc

	proxyConfig            *ProxyConfig
	setupNewDeviceCallback SetupNewDeviceCallback

	deviceRemote      *DeviceRemote
	proxyConfigResult *ProxyConfigResult
}

func NewProxyDeviceWithDefaults(proxyConfig *ProxyConfig, setupNewDeviceCallback SetupNewDeviceCallback) *ProxyDevice {
	return newProxyDevice(proxyConfig, setupNewDeviceCallback, DefaultProxyDeviceSettings())
}

func newProxyDevice(
	proxyConfig *ProxyConfig,
	setupNewDeviceCallback SetupNewDeviceCallback,
	settings *ProxyDeviceSettings,
) *ProxyDevice {
	ctx, cancel := context.WithCancel(context.Background())

	proxyDevice := &ProxyDevice{
		ctx:                    ctx,
		cancel:                 cancel,
		proxyConfig:            proxyConfig,
		setupNewDeviceCallback: setupNewDeviceCallback,
	}
	go connect.HandleError(proxyDevice.run, cancel)

	return proxyDevice
}

func (self *ProxyDevice) run() {

	// FIXME

	if self.setupNewDeviceCallback != nil {
		self.setupNewDeviceCallback.SetupNewDevice(nil, nil)
	}

	/*

		defer func() {
			REMOVEDEVICE.Close()
		}

		for {


			// connect to platform

			// if current client, check proxy config
			// if different or no current client, create new client, create new remote device
			// else set new transport into remote device

			// on read or write failure, retry

			if client == nil {
				CREATECLIENT()
				CREATEAPI()
				CREATEREMOTEDEVICE()

				if !setupNewDeviceCallback(remoteDevice, proxyConfigResult) {
					return
				}
			}


			connect := func() {
				ws, err := CONNECT()
				if err != nil {
					return err
				}

				config, err := ws.Read()
				if err != nil {
					return err
				}
				if config != EXPECTED {
					return configGoneError
				}
			}

			ws, configGone, err := connect()
			if configGone {
				if REMOTEDEVICE {
					REMOTEDEVICE.Close()
					REMOTEDEVICE = nil
				}
				continue
			}
			if err != nil {
				RETRYTIMEOUT
				continue
			}

			handleConnection := func() {

				remove := remoteDevice.SETTRANSPORT(WRAPPER)
				defer remove()

				// FIXME translate wrapper to websocket
			}

			handleConnection()
			RETRYTIMEOUT
		}
	*/
}

func (self *ProxyDevice) GetDevice() Device {
	return self.deviceRemote
}

func (self *ProxyDevice) GetProxyConfigResult() *ProxyConfigResult {
	return self.proxyConfigResult
}

func (self *ProxyDevice) Cancel() {
	self.deviceRemote.Cancel()
	self.cancel()
}

func (self *ProxyDevice) Close() {
	self.deviceRemote.Close()
	self.cancel()
}

func (self *ProxyDevice) GetDone() bool {
	select {
	case <-self.ctx.Done():
		return true
	default:
		return false
	}
}

// flow is create a new remote device, attach a transport
// on new connect (the device remote websocket), if the config of the device does not match passed, the client will disconnect with a missing proxy error
//    create a new client with proxy config, and call the new client setup function (which can just cancel the proxy device)
// on disconnect,
//    remove the client

// TODO remote device websocket transport with authentication
// TODO compile SDK to wasm
