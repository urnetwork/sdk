package sdk


// a proxy device is a device that is used through a proxy protocol - http or socks
// the device can be controlled through a normal `Device` interface;
// however, packet transport must use one of the proxy protocols
// "cloud proxy" is the typical use case of a proxy device, where the the packet routing is hosted on the platform
// ** note that with a cloud proxy, local packet routing is almost always disabled,
//    and a destination must be set in the device to route packets



// occasionally, due to extended network outage, a new device will need to be created
// this callback should either:
// - configure the new device state and return `true`
// - or return `false` to cancel the proxy device
// if the callback is not set, the default behavior is `true`
type SetupNewDeviceCallback interface {
	// return false to cancel the proxy device 
	SetupNewDevice(device Device, proxyConfigResult *ProxyConfigResult) bool
}



type ProxyConfig struct {
	LockCallerIp bool     `json:"lock_caller_ip"`
	LockIpList   []string `json:"lock_ip_list"`

	EnableSocks bool `json:"enable_socks"`
	EnableHttp bool `json:"enable_http"`
	HttpRequireAuth bool `json:"http_require_auth"`
}

type ProxyConfigResult struct {
	ExpirationTime   time.Time `json:"expiration_time"`
	KeepaliveSeconds int       `json:"keepalive_seconds"`
	HttpProxyUrl     string    `json:"http_proxy_url,omitempty"`
	SocksProxyUrl    string    `json:"socks_proxy_url,omitempty"`

	HttpProxyAuth *ProxyAuthResult `json:"http_proxy_auth"`
	SocksProxyAuth *ProxyAuthResult `json:"socks_proxy_auth"`
}

type ProxyAuthResult struct {
	Username     string   `json:"username"`
	Password string   `json:"password"`
}


type ProxyDeviceSettings struct {

	PlatformUrl string
	ApiUrl string
}



type ProxyDevice struct {
	ctx context.Context
	cancel context.CancelFunc


	deviceRemote *DeviceRemote
	proxyConfig *ProxyConfig
	setupNewDeviceCallback SetupNewDeviceCallback
}

func NewProxyDeviceWithDefaults(proxyConfig *ProxyConfig, setupNewDeviceCallback SetupNewDeviceCallback) *ProxyDevice {

	// FIXME set transport callback on remote device
}

func newProxyDevice(
	proxyConfig *ProxyConfig,
	setupNewDeviceCallback SetupNewDeviceCallback,
    settings *ProxyDeviceSettings,
) *ProxyDevice {

	// FIXME set transport callback on remote device
}

func (self *ProxyDevice) run() {

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
}



func (self *ProxyDevice) GetDevice() Device {

}

func (self *ProxyDevice) GetProxyConfigResult() *ProxyConfigResult {

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

}


// flow is create a new remote device, attach a transport 
// on new connect (the device remote websocket), if the config of the device does not match passed, the client will disconnect with a missing proxy error
//    create a new client with proxy config, and call the new client setup function (which can just cancel the proxy device)
// on disconnect, 
//    remove the client


// TODO remote device websocket transport with authentication
// TODO compile SDK to wasm




