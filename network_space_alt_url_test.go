package sdk

// The alt urls of a network space (EXTENDER.md L3, L4): the derivation from
// the platform url, the override, the family forms, and what they install into
// the two settings structs the space and its device build.

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
)

// A stored space derives its alt url from its platform url by the label rule,
// and the env prefix, the migration host and the family suffix all ride
// through the swap. The env secret path does not: only the host is dialed.
func TestNetworkSpaceAltUrl(t *testing.T) {
	ctx := context.Background()

	networkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("space.example", "main"),
		NetworkSpaceValues{},
		"",
	)
	defer networkSpace.close()
	connect.AssertEqual(t, networkSpace.GetAltUrl(), "wss://alt.space.example")
	connect.AssertEqual(t, networkSpace.GetAltUrlV4(), "wss://alt-v4.space.example")
	connect.AssertEqual(t, networkSpace.GetAltUrlV6(), "wss://alt-v6.space.example")

	// a non-main env prefixes the service label, and the env secret path is
	// below the host so it is dropped
	envNetworkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("space.example", "g2"),
		NetworkSpaceValues{EnvSecret: "secret"},
		"",
	)
	defer envNetworkSpace.close()
	connect.AssertEqual(t, envNetworkSpace.GetPlatformUrl(), "wss://g2-connect.space.example/secret")
	connect.AssertEqual(t, envNetworkSpace.GetAltUrl(), "wss://g2-alt.space.example")
	connect.AssertEqual(t, envNetworkSpace.GetAltUrlV4(), "wss://g2-alt-v4.space.example")
	connect.AssertEqual(t, envNetworkSpace.GetAltUrlV6(), "wss://g2-alt-v6.space.example")

	// a migration moves the whole namespace, the alt names with it
	migratedNetworkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("new.example", "main"),
		NetworkSpaceValues{MigrationHostName: "old.example"},
		"",
	)
	defer migratedNetworkSpace.close()
	connect.AssertEqual(t, migratedNetworkSpace.GetAltUrl(), "wss://alt.old.example")
	connect.AssertEqual(t, migratedNetworkSpace.GetAltUrlV4(), "wss://alt-v4.old.example")

	// an explicit platform url derives the same way, port included
	overrideNetworkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("custom.local", "main"),
		NetworkSpaceValues{
			ApiUrl:      "http://api.custom.example:8080/",
			PlatformUrl: "ws://connect.custom.example:5080/",
		},
		"",
	)
	defer overrideNetworkSpace.close()
	connect.AssertEqual(t, overrideNetworkSpace.GetAltUrl(), "ws://alt.custom.example:5080")
	connect.AssertEqual(t, overrideNetworkSpace.GetAltUrlV6(), "ws://alt-v6.custom.example:5080")

	// an ip-literal platform url has no `connect` label to replace, so the
	// space runs without alt rather than dialing a guess
	literalNetworkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("custom.local", "main"),
		NetworkSpaceValues{
			ApiUrl:      "https://127.0.0.1:8083",
			PlatformUrl: "wss://127.0.0.1:8080",
		},
		"",
	)
	defer literalNetworkSpace.close()
	connect.AssertEqual(t, literalNetworkSpace.GetAltUrl(), "")
	connect.AssertEqual(t, literalNetworkSpace.GetAltUrlV4(), "")
	connect.AssertEqual(t, literalNetworkSpace.GetAltUrlV6(), "")
}

// The configured override replaces the derivation entirely, which is what an
// in-process fixture and a one-off deployment use.
func TestNetworkSpaceAltUrlOverride(t *testing.T) {
	ctx := context.Background()

	networkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("space.example", "main"),
		NetworkSpaceValues{AltUrl: "https://127.0.0.1:14443/"},
		"",
	)
	defer networkSpace.close()
	connect.AssertEqual(t, networkSpace.GetAltUrl(), "https://127.0.0.1:14443")
	connect.AssertEqual(t, networkSpace.values.AltUrl, "https://127.0.0.1:14443/")
	// an ip literal has no label to suffix, so the pinned transports take the
	// override unchanged rather than a family name
	connect.AssertEqual(t, networkSpace.GetAltUrlV4(), "")
	connect.AssertEqual(t, networkSpace.GetAltUrlV6(), "")
	connect.AssertEqual(t, networkSpace.clientStrategySettings.AltUrl, "https://127.0.0.1:14443")

	// a named override keeps its own family forms
	namedNetworkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("space.example", "main"),
		NetworkSpaceValues{AltUrl: "https://edge.other.example"},
		"",
	)
	defer namedNetworkSpace.close()
	connect.AssertEqual(t, namedNetworkSpace.GetAltUrl(), "https://edge.other.example")
	connect.AssertEqual(t, namedNetworkSpace.GetAltUrlV4(), "https://edge-v4.other.example")
}

// A url-only space derives from its platform url exactly as a stored one does,
// so an embedder that names its endpoints reaches alt the same way (F1, L3).
func TestNetworkSpaceWithUrlsAltUrl(t *testing.T) {
	ctx := context.Background()

	networkSpace := NewNetworkSpaceWithUrls(
		ctx,
		"https://api.space.example",
		"wss://connect.space.example",
		connect.DefaultClientStrategySettings(),
	)
	defer networkSpace.close()
	connect.AssertEqual(t, networkSpace.GetAltUrl(), "wss://alt.space.example")
	connect.AssertEqual(t, networkSpace.GetAltUrlV4(), "wss://alt-v4.space.example")
	connect.AssertEqual(t, networkSpace.clientStrategySettings.AltUrl, "wss://alt.space.example")

	// the caller's own settings value is never written through
	callerSettings := connect.DefaultClientStrategySettings()
	sharedNetworkSpace := NewNetworkSpaceWithUrls(
		ctx,
		"https://api.space.example",
		"wss://connect.space.example",
		callerSettings,
	)
	defer sharedNetworkSpace.close()
	connect.AssertEqual(t, callerSettings.AltUrl, "")

	// the loopback harness shape derives nothing and keeps the strategy as the
	// caller configured it
	literalSettings := connect.DefaultClientStrategySettings()
	literalSettings.AltUrl = "https://127.0.0.1:14443"
	literalNetworkSpace := NewNetworkSpaceWithUrls(
		ctx,
		"https://127.0.0.1:8083",
		"wss://127.0.0.1:8080",
		literalSettings,
	)
	defer literalNetworkSpace.close()
	connect.AssertEqual(t, literalNetworkSpace.GetAltUrl(), "")
	connect.AssertEqual(t, literalNetworkSpace.clientStrategySettings.AltUrl, "https://127.0.0.1:14443")
}

// The space's client strategy carries the alt url, which is what adds the api's
// alt h3 and alt whodis dialers (L4). A space with no alt url leaves the
// strategy with the dialers it has always had.
func TestNetworkSpaceClientStrategySettingsAltUrl(t *testing.T) {
	ctx := context.Background()

	networkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("space.example", "main"),
		NetworkSpaceValues{},
		"",
	)
	defer networkSpace.close()
	connect.AssertEqual(t, networkSpace.clientStrategySettings.AltUrl, "wss://alt.space.example")

	literalNetworkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("custom.local", "main"),
		NetworkSpaceValues{
			ApiUrl:      "https://127.0.0.1:8083",
			PlatformUrl: "wss://127.0.0.1:8080",
		},
		"",
	)
	defer literalNetworkSpace.close()
	connect.AssertEqual(t, literalNetworkSpace.clientStrategySettings.AltUrl, "")
}

// The device's platform transport settings carry the alt url and leave the
// pump host empty so it derives from it (L3, L4). An embedder that named a
// pump host still wins, and a space with no alt url keeps the production
// default the pump has always had.
func TestDeviceLocalPlatformTransportSettingsAltUrl(t *testing.T) {
	target := ByteCount(24 * 1024 * 1024)
	newSettings := func(altUrl string, dnsPumpHost string) *connect.PlatformTransportSettings {
		return newDeviceLocalPlatformTransportSettings(
			target,
			connect.NewPlatformTransportBudgetForMemoryTarget(target),
			nil,
			altUrl,
			dnsPumpHost,
		)
	}

	settings := newSettings("wss://alt.space.example", "")
	connect.AssertEqual(t, settings.AltUrl, "wss://alt.space.example")
	connect.AssertEqual(t, settings.DnsPumpHost, "")

	// whitespace is a blank setting, not a host
	blankSettings := newSettings("   ", "")
	connect.AssertEqual(t, blankSettings.AltUrl, "")
	connect.AssertEqual(t, blankSettings.DnsPumpHost, connect.DefaultDnsPumpHost)

	// the integration host's provisioned pump ingress outranks the derivation
	pumpSettings := newSettings("wss://alt.space.example", "127.0.1.7")
	connect.AssertEqual(t, pumpSettings.AltUrl, "wss://alt.space.example")
	connect.AssertEqual(t, pumpSettings.DnsPumpHost, "127.0.1.7")

	// no alt deployment: every carrier stays where it has always been
	plainSettings := newSettings("", "")
	connect.AssertEqual(t, plainSettings.AltUrl, "")
	connect.AssertEqual(t, plainSettings.DnsPumpHost, connect.DefaultDnsPumpHost)
}
