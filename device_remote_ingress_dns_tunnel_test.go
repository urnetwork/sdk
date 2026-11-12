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
// material. The first three use only the apps' own calls (the space setters),
// so they ran on the code before the fix, where the tunnel's space kept the
// values it started with.

const ingressDnsTestWait = 10 * time.Second

// An app process and a tunnel process: the app's space, the tunnel's own
// space, and the remote and device synced over the rpc. `beforeAttach` runs
// once the device is up and before the app's remote exists.
func newIngressDnsTestTunnel(
	t *testing.T,
	beforeAttach func(appSpace *NetworkSpace),
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
		beforeAttach(appSpace)
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

// Waits for `done`, which reads state another goroutine sets; the wait ends at
// the first true, so a passing run takes only as long as the hand-off.
func waitForIngressDnsTest(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(ingressDnsTestWait)
	for !done() {
		if deadline.Before(time.Now()) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
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

	if countryCode := appSpace.SetIngressCountryCode("cn"); countryCode != "cn" {
		t.Fatalf("stored region = %q", countryCode)
	}

	waitForIngressDnsTest(t, "the running tunnel's strategy to take China", func() bool {
		return tunnelSpace.clientStrategy.DohSettings().IngressCountryCode == "cn"
	})
	if countryCode := tunnelSpace.GetIngressCountryCode(); countryCode != "cn" {
		t.Fatalf("the tunnel's space region = %q", countryCode)
	}
	if !slices.Equal(tunnelSpace.clientStrategy.DohSettings().DnsResolverSettings.RemoteDohUrlsIpv4, ingressDnsTestChinaDohUrlsIpv4()) {
		t.Fatalf("the tunnel resolves its names over %v, expected China's servers",
			tunnelSpace.clientStrategy.DohSettings().DnsResolverSettings.RemoteDohUrlsIpv4)
	}
	if tunnelSpace.clientStrategy != tunnelStrategy {
		t.Fatal("the tunnel's strategy was replaced: the change must apply in place")
	}

	// back to undisclosed, which the tunnel takes the same way
	appSpace.SetIngressCountryCode("")
	waitForIngressDnsTest(t, "the running tunnel to go back to undisclosed", func() bool {
		return tunnelSpace.clientStrategy.DohSettings().IngressCountryCode == "" &&
			tunnelSpace.GetIngressCountryCode() == ""
	})
}

// The same for the bootstrap DoH servers a user names: they reach the
// running tunnel ahead of the defaults, in place.
func TestDeviceRemoteHandsTheBootstrapDohServersToARunningTunnel(t *testing.T) {
	appSpace, tunnelSpace, _ := newIngressDnsTestTunnel(t, nil)
	dohUrlsIpv4 := []string{"https://192.0.2.1/dns-query", "https://192.0.2.2/dns-query"}
	dohUrlIpv6 := "https://[2001:db8::1]/dns-query"

	if errorId := appSpace.SetControlDohUrls(testStringList(append(slices.Clone(dohUrlsIpv4), dohUrlIpv6)...)); errorId != "" {
		t.Fatal(errorId)
	}

	waitForIngressDnsTest(t, "the running tunnel to take the bootstrap servers", func() bool {
		return slices.Equal(tunnelSpace.GetControlDohUrlsIpv4().getAll(), dohUrlsIpv4) &&
			slices.Equal(tunnelSpace.GetControlDohUrlsIpv6().getAll(), []string{dohUrlIpv6})
	})
	inForce := tunnelSpace.clientStrategy.DohSettings().DnsResolverSettings
	if len(inForce.RemoteDohUrlsIpv4) < len(dohUrlsIpv4) || !slices.Equal(inForce.RemoteDohUrlsIpv4[:len(dohUrlsIpv4)], dohUrlsIpv4) {
		t.Fatalf("the tunnel's v4 servers in force = %v, expected %v first", inForce.RemoteDohUrlsIpv4, dohUrlsIpv4)
	}
	if len(inForce.RemoteDohUrlsIpv6) < 1 || inForce.RemoteDohUrlsIpv6[0] != dohUrlIpv6 {
		t.Fatalf("the tunnel's v6 servers in force = %v, expected %s first", inForce.RemoteDohUrlsIpv6, dohUrlIpv6)
	}
}

// An app that attaches to a tunnel already running (a relaunch, or the rpc
// reconnecting) gives it the region the app saved meanwhile: the tunnel
// started before the change and never saw it.
func TestDeviceRemoteGivesARunningTunnelTheRegionSavedBeforeItAttached(t *testing.T) {
	_, tunnelSpace, _ := newIngressDnsTestTunnel(t, func(appSpace *NetworkSpace) {
		if countryCode := appSpace.SetIngressCountryCode("cn"); countryCode != "cn" {
			t.Fatalf("stored region = %q", countryCode)
		}
	})

	waitForIngressDnsTest(t, "the attached tunnel to take China", func() bool {
		return tunnelSpace.clientStrategy.DohSettings().IngressCountryCode == "cn" &&
			tunnelSpace.GetIngressCountryCode() == "cn"
	})
}
