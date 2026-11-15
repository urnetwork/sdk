//go:build !ios_extension && !js

package sdk

import (
	"context"
	"testing"

	"github.com/urnetwork/sidecar"
)

// The forwarding client's urls, which the tunnel derives without sidecar's host list, are the sidecar role's for the
// same network space.
func TestForwardUrlsMatchTheHostList(t *testing.T) {
	for _, apiUrl := range []string{"https://api.bringyour.example", "https://test-api.bringyour.example:8443/v1"} {
		space := newNetworkSpace(context.Background(), *NewNetworkSpaceKey("forward-urls.test", "test"),
			NetworkSpaceValues{ApiUrl: apiUrl, PlatformUrl: "wss://platform.bringyour.example"}, t.TempDir())
		roleSettings := sidecar.DefaultSidecarSettings()
		if err := applySidecarServiceUrls(space, roleSettings); err != nil {
			t.Fatal(err)
		}
		forwardSettings, err := deviceForwardApiSettings(space.GetApiUrl(), space.GetPlatformUrl())
		if err != nil {
			t.Fatal(err)
		}
		for name, pair := range map[string][2]string{
			"api":     {roleSettings.ApiUrl, forwardSettings.ApiUrl},
			"auth":    {roleSettings.AuthUrl, forwardSettings.AuthUrl},
			"evs":     {roleSettings.EvsUrl, forwardSettings.EvsUrl},
			"gkm":     {roleSettings.GkmUrl, forwardSettings.GkmUrl},
			"sockets": {roleSettings.SocketsUrl, forwardSettings.SocketsUrl},
			"dns":     {roleSettings.DnsUrl, forwardSettings.DnsUrl},
		} {
			if pair[0] != pair[1] {
				t.Errorf("%s: the role's %q, the forwarding client's %q", name, pair[0], pair[1])
			}
		}
		space.close()
	}
	if _, err := deviceForwardApiSettings("http://api.bringyour.example", ""); err == nil {
		t.Error("a plain-http api url took a forwarding client")
	}
}
