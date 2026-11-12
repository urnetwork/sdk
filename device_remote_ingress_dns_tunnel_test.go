package sdk

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The connection region and bootstrap DoH servers the app saves reach a
// running tunnel's device, whose process runs its own copy of the network
// space (device_remote_ingress_dns.go, support inbox 8051).
//
// The tunnel tests stand in for the ios and macOS packet tunnel and the
// desktop services: the app's DeviceRemote is bound to the app's space, and
// the device runs in a second space of its own, as the tunnel process imports
// one, the two paired over the real rpc on a free port with their own key
// material. The app changes its space with the setters the apps call. The
// tunnel space's own change callback is the barrier: it fires when the device
// applies the values, so each test waits for that event and then reads the
// state, and the deadline only bounds a failure.

// Bounds a wait for an event that should come; a passing run never waits it.
const ingressDnsTestWait = 10 * time.Second

// An app process and a tunnel process: the app's space, the tunnel's own
// space, and the remote and device synced over the rpc. `beforeAttach` runs
// once the device is up and before the app's remote exists.
func newIngressDnsTestTunnel(
	t *testing.T,
	beforeAttach func(appSpace *NetworkSpace, tunnelSpace *NetworkSpace),
) (appSpace *NetworkSpace, tunnelSpace *NetworkSpace, deviceRemote *DeviceRemote) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	appSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("app space: %v", err)
	}
	tunnelSpace, _, err = testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("tunnel space: %v", err)
	}

	clientId := connect.NewId()
	instanceId := NewId()
	deviceLocalSettings := DefaultDeviceLocalSettings()
	deviceLocalSettings.Verbose = false
	deviceLocal, err := newDeviceLocalWithOverrides(
		tunnelSpace, byJwt, "", "", "", instanceId, deviceLocalSettings, clientId,
	)
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	t.Cleanup(deviceLocal.Close)

	keyMaterial, err := GenerateDeviceRpcKeyMaterial()
	if err != nil {
		t.Fatalf("key material: %v", err)
	}
	hostPort := testing_freeHostPort()
	if err := deviceLocal.SetRpcServer(keyMaterial.GetServerPem(), keyMaterial.GetClientCertPem(), hostPort); err != nil {
		t.Fatalf("rpc server: %v", err)
	}
	if !testing_waitDialable(hostPort, true, ingressDnsTestWait) {
		t.Fatal("the device's rpc server is not dialable")
	}

	if beforeAttach != nil {
		beforeAttach(appSpace, tunnelSpace)
	}

	settings := defaultDeviceRpcSettings()
	dialer := NewWebsocketDeviceRpcDialer(
		requireRemoteAddress(hostPort), keyMaterial.GetClientPem(), keyMaterial.GetServerCertPem(), settings,
	)
	deviceRemote, err = newDeviceRemoteWithOverrides(appSpace, byJwt, instanceId, settings, clientId, dialer)
	if err != nil {
		t.Fatalf("device remote: %v", err)
	}
	t.Cleanup(deviceRemote.Close)

	deviceRemote.Sync()
	if !deviceRemote.waitForSync(ingressDnsTestWait) {
		t.Fatal("the device remote did not sync")
	}
	if !deviceRemote.GetRemoteConnected() {
		t.Fatal("the device remote is not connected after its sync")
	}
	return
}

// The ingress dns snapshots a space takes in place from now on, in order.
func ingressDnsTestChanges(t *testing.T, networkSpace *NetworkSpace) chan *DeviceRemoteIngressDns {
	t.Helper()
	changes := make(chan *DeviceRemoteIngressDns, 16)
	unsub := networkSpace.addIngressDnsChangeCallback(func(ingressDns *DeviceRemoteIngressDns, revision uint64) {
		// a callback must not block; the tests make fewer changes than fit
		select {
		case changes <- ingressDns:
		default:
		}
	})
	t.Cleanup(unsub)
	return changes
}

// Waits for the change `want` accepts. Changes before it are skipped.
func waitForIngressDnsTestChange(
	t *testing.T,
	changes chan *DeviceRemoteIngressDns,
	what string,
	want func(ingressDns *DeviceRemoteIngressDns) bool,
) {
	t.Helper()
	deadline := time.After(ingressDnsTestWait)
	for {
		select {
		case ingressDns := <-changes:
			if want(ingressDns) {
				return
			}
		case <-deadline:
			t.Fatalf("the tunnel's space never took %s", what)
		}
	}
}

// China's ingress DoH servers, as a space with that region puts them in force.
func ingressDnsTestChinaDohUrlsIpv4() []string {
	return connect.IngressDohSettings("cn", nil, nil).DnsResolverSettings.RemoteDohUrlsIpv4
}

// The root cause: the user chooses China as the connection region while the
// tunnel is up (the "Can't reach URnetwork" prompt over a stalled connect).
// The app's space takes it in place, but the tunnel's device resolves
// URnetwork's own names in its own space, which kept the region it started
// with until a disconnect and connect.
func TestDeviceRemoteHandsTheConnectionRegionToARunningTunnel(t *testing.T) {
	appSpace, tunnelSpace, _ := newIngressDnsTestTunnel(t, nil)
	if countryCode := tunnelSpace.GetIngressCountryCode(); countryCode != "" {
		t.Fatalf("the tunnel starts with region %q, expected undisclosed", countryCode)
	}
	tunnelStrategy := tunnelSpace.clientStrategy
	tunnelChanges := ingressDnsTestChanges(t, tunnelSpace)

	if countryCode := appSpace.SetIngressCountryCode("cn"); countryCode != "cn" {
		t.Fatalf("stored region = %q", countryCode)
	}
	waitForIngressDnsTestChange(t, tunnelChanges, "China", func(ingressDns *DeviceRemoteIngressDns) bool {
		return ingressDns.CountryCode == "cn"
	})
	if countryCode := tunnelSpace.clientStrategy.DohSettings().IngressCountryCode; countryCode != "cn" {
		t.Fatalf("the tunnel's strategy region = %q", countryCode)
	}
	if dohUrlsIpv4 := tunnelSpace.clientStrategy.DohSettings().DnsResolverSettings.RemoteDohUrlsIpv4; !slices.Equal(dohUrlsIpv4, ingressDnsTestChinaDohUrlsIpv4()) {
		t.Fatalf("the tunnel resolves its names over %v, expected China's servers", dohUrlsIpv4)
	}
	if tunnelSpace.clientStrategy != tunnelStrategy {
		t.Fatal("the tunnel's strategy was replaced: the change must apply in place")
	}

	// back to undisclosed, which the tunnel takes the same way
	appSpace.SetIngressCountryCode("")
	waitForIngressDnsTestChange(t, tunnelChanges, "undisclosed", func(ingressDns *DeviceRemoteIngressDns) bool {
		return ingressDns.CountryCode == ""
	})
	if countryCode := tunnelSpace.clientStrategy.DohSettings().IngressCountryCode; countryCode != "" {
		t.Fatalf("the tunnel's strategy region = %q, expected undisclosed", countryCode)
	}
}

// The same for the bootstrap DoH servers a user names: they reach the
// running tunnel ahead of the defaults, in place.
func TestDeviceRemoteHandsTheBootstrapDohServersToARunningTunnel(t *testing.T) {
	appSpace, tunnelSpace, _ := newIngressDnsTestTunnel(t, nil)
	dohUrlsIpv4 := []string{"https://192.0.2.1/dns-query", "https://192.0.2.2/dns-query"}
	dohUrlIpv6 := "https://[2001:db8::1]/dns-query"
	tunnelChanges := ingressDnsTestChanges(t, tunnelSpace)

	if errorId := appSpace.SetControlDohUrls(testStringList(append(slices.Clone(dohUrlsIpv4), dohUrlIpv6)...)); errorId != "" {
		t.Fatal(errorId)
	}
	waitForIngressDnsTestChange(t, tunnelChanges, "the bootstrap servers", func(ingressDns *DeviceRemoteIngressDns) bool {
		return slices.Equal(ingressDns.ControlDohUrlsIpv4, dohUrlsIpv4) &&
			slices.Equal(ingressDns.ControlDohUrlsIpv6, []string{dohUrlIpv6})
	})
	inForceSettings := tunnelSpace.clientStrategy.DohSettings().DnsResolverSettings
	if len(inForceSettings.RemoteDohUrlsIpv4) < len(dohUrlsIpv4) || !slices.Equal(inForceSettings.RemoteDohUrlsIpv4[:len(dohUrlsIpv4)], dohUrlsIpv4) {
		t.Fatalf("the tunnel's v4 servers in force = %v, expected %v first", inForceSettings.RemoteDohUrlsIpv4, dohUrlsIpv4)
	}
	if len(inForceSettings.RemoteDohUrlsIpv6) < 1 || inForceSettings.RemoteDohUrlsIpv6[0] != dohUrlIpv6 {
		t.Fatalf("the tunnel's v6 servers in force = %v, expected %s first", inForceSettings.RemoteDohUrlsIpv6, dohUrlIpv6)
	}
}

// An app that attaches to a tunnel already running (a relaunch, or the rpc
// reconnecting) gives it the region the app saved meanwhile: the tunnel
// started before the change and never saw it. The first sync applies it, so
// the change is in when the helper's sync returns.
func TestDeviceRemoteGivesARunningTunnelTheRegionSavedBeforeItAttached(t *testing.T) {
	var tunnelChanges chan *DeviceRemoteIngressDns
	_, tunnelSpace, _ := newIngressDnsTestTunnel(t, func(appSpace *NetworkSpace, tunnelSpace *NetworkSpace) {
		tunnelChanges = ingressDnsTestChanges(t, tunnelSpace)
		if countryCode := appSpace.SetIngressCountryCode("cn"); countryCode != "cn" {
			t.Fatalf("stored region = %q", countryCode)
		}
	})

	waitForIngressDnsTestChange(t, tunnelChanges, "China", func(ingressDns *DeviceRemoteIngressDns) bool {
		return ingressDns.CountryCode == "cn"
	})
	if countryCode := tunnelSpace.clientStrategy.DohSettings().IngressCountryCode; countryCode != "cn" {
		t.Fatalf("the attached tunnel's strategy region = %q", countryCode)
	}
}
