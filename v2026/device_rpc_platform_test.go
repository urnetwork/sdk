package sdk

import (
	"net/url"
	"testing"
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
		{
			name:   "bare host defaults to wss",
			in:     "proxy.example.com",
			scheme: "wss",
			host:   "proxy.example.com",
		},
		{
			name:   "host:port defaults to wss",
			in:     "proxy.example.com:8443",
			scheme: "wss",
			host:   "proxy.example.com:8443",
		},
		{
			name:   "wss passthrough",
			in:     "wss://proxy.example.com",
			scheme: "wss",
			host:   "proxy.example.com",
		},
		{
			name:   "ws passthrough",
			in:     "ws://127.0.0.1:7500",
			scheme: "ws",
			host:   "127.0.0.1:7500",
		},
		{
			name:   "https upgrades to wss",
			in:     "https://proxy.example.com",
			scheme: "wss",
			host:   "proxy.example.com",
		},
		{
			name:   "http downgrades to ws",
			in:     "http://127.0.0.1:7500",
			scheme: "ws",
			host:   "127.0.0.1:7500",
		},
	}

	for _, c := range cases {
		s, err := deviceRpcUrl(c.in, signed)
		if err != nil {
			t.Errorf("%s: deviceRpcUrl: %v", c.name, err)
			continue
		}

		u, err := url.Parse(s)
		if err != nil {
			t.Errorf("%s: parse %q: %v", c.name, s, err)
			continue
		}
		if u.Scheme != c.scheme {
			t.Errorf("%s: scheme = %q, want %q", c.name, u.Scheme, c.scheme)
		}
		if u.Host != c.host {
			t.Errorf("%s: host = %q, want %q", c.name, u.Host, c.host)
		}
		// the path is always /device-rpc
		if u.Path != "/device-rpc" {
			t.Errorf("%s: path = %q, want /device-rpc", c.name, u.Path)
		}
		// the signed proxy id round-trips through the query encoding intact,
		// including the base64 specials
		if proxy := u.Query().Get("proxy"); proxy != signed {
			t.Errorf("%s: proxy = %q, want %q", c.name, proxy, signed)
		}
	}
}
