package sdk

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
)

// the family-pinned urls derive by suffixing the service label (IPV6.md A9):
// the env prefix and the domain stay put, the scheme, port and env-secret path
// are preserved, and anything without a label to suffix yields "" so the
// pinned transports are disabled rather than aimed at a made-up name
func TestFamilyServiceUrl(t *testing.T) {
	cases := []struct {
		url string
		v4  string
		v6  string
	}{
		{"wss://connect.bringyour.com", "wss://connect-v4.bringyour.com", "wss://connect-v6.bringyour.com"},
		{"https://api.bringyour.com", "https://api-v4.bringyour.com", "https://api-v6.bringyour.com"},
		{"wss://connect.ur.network", "wss://connect-v4.ur.network", "wss://connect-v6.ur.network"},
		{"wss://g2-connect.bringyour.com", "wss://g2-connect-v4.bringyour.com", "wss://g2-connect-v6.bringyour.com"},
		{"https://beta-api.example.com/secret", "https://beta-api-v4.example.com/secret", "https://beta-api-v6.example.com/secret"},
		{"ws://connect.custom.test:5080", "ws://connect-v4.custom.test:5080", "ws://connect-v6.custom.test:5080"},
		{"http://api.custom.test:8080/env", "http://api-v4.custom.test:8080/env", "http://api-v6.custom.test:8080/env"},
		// no label to suffix
		{"wss://127.0.0.1:8080", "", ""},
		{"wss://[::1]:8080", "", ""},
		{"wss://localhost:8080", "", ""},
		{"", "", ""},
		{"not a url", "", ""},
		// already pinned by the operator
		{"wss://connect-v4.bringyour.com", "", ""},
		{"wss://connect-v6.bringyour.com", "", ""},
	}
	for _, c := range cases {
		connect.AssertEqual(t, familyServiceUrl(c.url, 4), c.v4)
		connect.AssertEqual(t, familyServiceUrl(c.url, 6), c.v6)
		// only 4 and 6 are families
		connect.AssertEqual(t, familyServiceUrl(c.url, 5), "")
	}
}

func TestNetworkSpaceFamilyUrls(t *testing.T) {
	ctx := context.Background()

	networkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("bringyour.com", "main"),
		NetworkSpaceValues{},
		"",
	)
	connect.AssertEqual(t, networkSpace.GetPlatformUrlV4(), "wss://connect-v4.bringyour.com")
	connect.AssertEqual(t, networkSpace.GetPlatformUrlV6(), "wss://connect-v6.bringyour.com")
	connect.AssertEqual(t, networkSpace.GetApiUrlV4(), "https://api-v4.bringyour.com")
	connect.AssertEqual(t, networkSpace.GetApiUrlV6(), "https://api-v6.bringyour.com")
	connect.AssertEqual(t, networkSpace.HasPlatformFamilyUrls(), true)
	networkSpace.close()

	// an env prefix and an env secret: the suffix lands on the service label
	// and the secret path survives
	secretNetworkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("ur.network", "g2"),
		NetworkSpaceValues{EnvSecret: "secret"},
		"",
	)
	connect.AssertEqual(t, secretNetworkSpace.GetPlatformUrlV4(), "wss://g2-connect-v4.ur.network/secret")
	connect.AssertEqual(t, secretNetworkSpace.GetPlatformUrlV6(), "wss://g2-connect-v6.ur.network/secret")
	connect.AssertEqual(t, secretNetworkSpace.GetApiUrlV6(), "https://g2-api-v6.ur.network/secret")
	connect.AssertEqual(t, secretNetworkSpace.HasPlatformFamilyUrls(), true)
	secretNetworkSpace.close()

	// a migration host name derives against the migrated domain
	migratedNetworkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("ur.network", "main"),
		NetworkSpaceValues{MigrationHostName: "bringyour.com"},
		"",
	)
	connect.AssertEqual(t, migratedNetworkSpace.GetPlatformUrlV4(), "wss://connect-v4.bringyour.com")
	migratedNetworkSpace.close()

	// explicit overrides derive from the override verbatim, port included
	overrideNetworkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("custom.local", "main"),
		NetworkSpaceValues{
			ApiUrl:      "http://api.custom.test:8080/",
			PlatformUrl: "ws://connect.custom.test:5080/",
		},
		"",
	)
	connect.AssertEqual(t, overrideNetworkSpace.GetPlatformUrlV4(), "ws://connect-v4.custom.test:5080")
	connect.AssertEqual(t, overrideNetworkSpace.GetApiUrlV6(), "http://api-v6.custom.test:8080")
	connect.AssertEqual(t, overrideNetworkSpace.HasPlatformFamilyUrls(), true)
	overrideNetworkSpace.close()

	// an ip-literal override (the local test harness shape) has no family
	// urls, so the pinned transports stay off
	literalNetworkSpace := NewNetworkSpaceWithUrls(
		ctx,
		"https://127.0.0.1:8083",
		"wss://127.0.0.1:8080",
		connect.DefaultClientStrategySettings(),
	)
	connect.AssertEqual(t, literalNetworkSpace.GetPlatformUrlV4(), "")
	connect.AssertEqual(t, literalNetworkSpace.GetPlatformUrlV6(), "")
	connect.AssertEqual(t, literalNetworkSpace.GetApiUrlV4(), "")
	connect.AssertEqual(t, literalNetworkSpace.HasPlatformFamilyUrls(), false)
	literalNetworkSpace.close()
}
