package sdk

import (
	"context"
	"net"
	"net/url"

	"github.com/urnetwork/connect"
)

// PlatformDeviceRpcDialer connects a DeviceRemote directly to the proxy host
// that runs the hosted DeviceLocal, at ws(s)://<proxyHost>/device-rpc. The
// DeviceLocal lives on the proxy host, so there is no connect-service or
// resident hop.
//
// Auth is the device's signed proxy id, passed as the `proxy` query parameter
// (a browser WebSocket cannot set request headers, but can set query params).
// The signed proxy id is an HMAC bearer token — the same credential the wg and
// https data planes use — so no JWT and no auth handshake are needed. The
// forward and reverse rpc streams are multiplexed over the single connection by
// the shared deviceRpcMux.
//
// The concrete websocket open is provided per platform by dialDeviceRpcWs:
// gorilla on native builds, the browser WebSocket on js.
type PlatformDeviceRpcDialer struct {
	proxyUrl      string
	signedProxyId string
	settings      *deviceRpcSettings
	log           connect.Logger
}

// compile check that PlatformDeviceRpcDialer conforms to deviceRpcDialer
var _ deviceRpcDialer = (*PlatformDeviceRpcDialer)(nil)

// NewPlatformDeviceRpcDialer dials the proxy host. proxyUrl may be a bare host
// (proxy host name), host:port, or ws/wss url; a bare host defaults to wss.
func NewPlatformDeviceRpcDialer(
	proxyUrl string,
	signedProxyId string,
	settings *deviceRpcSettings,
) *PlatformDeviceRpcDialer {
	return &PlatformDeviceRpcDialer{
		proxyUrl:      proxyUrl,
		signedProxyId: signedProxyId,
		settings:      settings,
		log:           settings.logger(),
	}
}

func (self *PlatformDeviceRpcDialer) Dial(ctx context.Context) (net.Conn, net.Conn, error) {
	ws, err := dialDeviceRpcWs(ctx, self.proxyUrl, self.signedProxyId, self.settings)
	if err != nil {
		self.log.Infof("[dr]device rpc dial %s err = %s", self.proxyUrl, err)
		return nil, nil, err
	}
	self.log.Infof("[dr]device rpc dial %s connected", self.proxyUrl)
	mux := newDeviceRpcMux(ctx, ws, self.settings)
	return mux.conns[deviceRpcStreamForward], mux.conns[deviceRpcStreamReverse], nil
}

// deviceRpcUrl normalizes a proxy url to a ws/wss url with the /device-rpc path
// and the signed proxy id as the `proxy` query parameter. A bare host or
// host:port defaults to wss.
func deviceRpcUrl(proxyUrl string, signedProxyId string) (string, error) {
	s := proxyUrl
	switch {
	case hasScheme(s, "wss://"), hasScheme(s, "ws://"):
		// as-is
	case hasScheme(s, "https://"):
		s = "wss://" + s[len("https://"):]
	case hasScheme(s, "http://"):
		s = "ws://" + s[len("http://"):]
	default:
		s = "wss://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	u.Path = "/device-rpc"
	u.RawQuery = url.Values{"proxy": {signedProxyId}}.Encode()
	return u.String(), nil
}

func hasScheme(s string, scheme string) bool {
	return len(s) >= len(scheme) && s[:len(scheme)] == scheme
}
