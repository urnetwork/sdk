package sdk

import (
	"net/url"
	"testing"

	"github.com/urnetwork/connect"
)

// TestDeviceRpcUrl covers deviceRpcUrl, which normalizes a proxy host into the
// wss device-rpc url a DeviceRemote dials and carries the signed proxy id as the
// `proxy` query parameter (the browser cannot set request headers, so the token
// must ride in the url).
func TestDeviceRpcUrl(t *testing.T) {
	// a token with base64 specials (+ / =) so the query-encoding is exercised
	const signed = "aGVsbG8+d29ybGQ/eA=="

	cases := []struct {
		name   string
		in     string
		scheme string
		host   string
	}{
		{"bare host defaults to wss", "proxy.example.com", "wss", "proxy.example.com"},
		{"host:port defaults to wss", "proxy.example.com:8443", "wss", "proxy.example.com:8443"},
		{"wss passthrough", "wss://proxy.example.com", "wss", "proxy.example.com"},
		{"ws passthrough", "ws://127.0.0.1:7500", "ws", "127.0.0.1:7500"},
		{"https upgrades to wss", "https://proxy.example.com", "wss", "proxy.example.com"},
		{"http downgrades to ws", "http://127.0.0.1:7500", "ws", "127.0.0.1:7500"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := deviceRpcUrl(c.in, signed)
			connect.AssertEqual(t, err, nil)

			u, err := url.Parse(s)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, u.Scheme, c.scheme)
			connect.AssertEqual(t, u.Host, c.host)
			// the path is always /device-rpc
			connect.AssertEqual(t, u.Path, "/device-rpc")
			// the signed proxy id round-trips through the query encoding intact,
			// including the base64 specials
			connect.AssertEqual(t, u.Query().Get("proxy"), signed)
		})
	}
}
