//go:build !js

package sdk

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urnetwork/connect"
)

// Native callers can safely perform synchronous rpc reads and writes; keeping
// the live service available also preserves cross-remote state visibility.
const platformDeviceRpcBrowserStateOnly = false

// dialDeviceRpcWs opens the proxy-host binary message carrier. Native auth is
// the signed proxy id in Authorization. An enabled cohort prefers FramerXl and
// falls back to a fresh Gorilla WebSocket on capability failure. No auth frame
// is needed after101, so the mux starts immediately. A bare host defaults to wss.
func dialDeviceRpcWs(ctx context.Context, proxyUrl string, signedProxyId string, settings *deviceRpcSettings) (deviceRpcWs, error) {
	s, err := deviceRpcUrl(proxyUrl, signedProxyId)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	// Native clients can use a header, keeping bearer credentials out of
	// request targets and intermediary access logs. Browser URLs retain the
	// existing query form because their WebSocket API cannot set headers.
	u.RawQuery = ""
	header := http.Header{"Authorization": []string{"Bearer " + signedProxyId}}

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

	carrier, err := connect.DialH1Messages(ctx, u.String(), header, dialer, connect.H1FramerXlProtocol, int(settings.maxFrameBytes()), settings.EnableH1Plus, settings.H1PlusStats)
	if err != nil {
		return nil, err
	}
	ws := carrier.(deviceRpcWs)
	// clear any handshake deadlines; the mux manages its own
	ws.SetWriteDeadline(time.Time{})
	ws.SetReadDeadline(time.Time{})
	return ws, nil
}

// interface assertion is here (native) because the js build has no gorilla type
var _ deviceRpcWs = (*websocket.Conn)(nil)
