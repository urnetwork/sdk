package sdk

import "time"

// Proxy wire models are shared with auth APIs on every platform. The hosted
// proxy lifecycle itself is a Go-embedder API in proxy_device.go.

func DefaultProxyConfig() *ProxyConfig {
	return &ProxyConfig{
		EnableHttp: true,
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
