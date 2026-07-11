//go:build !js

package sdk

import (
	"context"
	"crypto/tls"
	"net"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
)

// dialDeviceRpcWs opens the proxy-host device-rpc websocket with gorilla. Auth
// is the signed proxy id carried in the url (the `proxy` query parameter);
// there is no auth frame, so the mux starts immediately. proxyUrl may be a bare
// host, a host:port, or a ws/wss url; a bare host defaults to wss.
func dialDeviceRpcWs(ctx context.Context, proxyUrl string, signedProxyId string, settings *deviceRpcSettings) (deviceRpcWs, error) {
	s, err := deviceRpcUrl(proxyUrl, signedProxyId)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}

	netDialer := &net.Dialer{
		Timeout:         settings.RpcConnectTimeout,
		KeepAliveConfig: deviceRpcKeepAliveConfig(settings),
	}
	dialer := &websocket.Dialer{
		HandshakeTimeout: settings.RpcConnectTimeout,
		NetDialContext:   netDialer.DialContext,
	}
	if u.Scheme == "wss" {
		dialer.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	}

	ws, _, err := dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return nil, err
	}
	// clear any handshake deadlines; the mux manages its own
	ws.SetWriteDeadline(time.Time{})
	ws.SetReadDeadline(time.Time{})
	return ws, nil
}

// interface assertion is here (native) because the js build has no gorilla type
var _ deviceRpcWs = (*websocket.Conn)(nil)
