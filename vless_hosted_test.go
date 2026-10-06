package sdk

// A hosted (cloud) device never takes VLESS, which is not cloud safe: a VLESS
// server would be dialed from the host. These tests cover each path by which
// VLESS could reach one: the space it is built on, its own strategy, and the
// space a cloud host shares among its hosted devices. The device rpc, which
// carries no VLESS, is pinned with the other user-named endpoints
// (hosted_user_endpoints_test.go).

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A hosted device built on a space that routes through a VLESS server holds no
// VLESS dialer: not the space's server, not one the space names later, and not
// one added to its own strategy. Its requests never reach the server. A device
// on the same space that is not hosted still routes through it.
func TestHostedDeviceRefusesVless(t *testing.T) {
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello api"))
	}))
	defer api.Close()
	rootCas := x509.NewCertPool()
	rootCas.AddCert(api.Certificate())
	relay := newTestVlessRelay(t)

	// the api is reachable only through the VLESS server
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.EnableNormal = false
	strategySettings.EnableResilient = false
	strategySettings.ExposeServerIps = false
	strategySettings.ExposeServerHostNames = false
	strategySettings.ConnectSettings.TlsConfig = &tls.Config{RootCAs: rootCas}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace := NewNetworkSpaceWithUrls(ctx, api.URL, "wss://127.0.0.1:1", strategySettings)
	defer networkSpace.close()
	vlessSettings := &VlessSettings{
		Enabled:  true,
		Address:  "127.0.0.1",
		Port:     relay.port(),
		Id:       testVlessId,
		Network:  connect.VlessNetworkTcp,
		Security: connect.VlessSecurityNone,
	}
	if errorId := networkSpace.SetVlessSettings(vlessSettings); errorId != "" {
		t.Fatal(errorId)
	}

	newDevice := func(hostedIncompatible bool) *DeviceLocal {
		settings := DefaultDeviceLocalSettings()
		settings.AllowProvider = false
		settings.DisableLogging = true
		settings.HostedIncompatible = hostedIncompatible
		device, err := newDeviceLocalWithOverrides(
			networkSpace, "synthetic-device-token", "test", "test", "test", NewId(), settings, connect.NewId(),
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			device.CloseAndWait(context.Background())
		})
		return device
	}
	get := func(strategy *connect.ClientStrategy, timeout time.Duration) (string, error) {
		requestCtx, requestCancel := context.WithTimeout(ctx, timeout)
		defer requestCancel()
		body, err := connect.HttpGetWithStrategyRaw(requestCtx, strategy, api.URL+"/hello", "")
		return string(body), err
	}

	device := newDevice(false)
	body, err := get(device.clientStrategy, 30*time.Second)
	if err != nil {
		t.Fatalf("the device that is not hosted, through the VLESS server: %s", err)
	}
	if body != "hello api" {
		t.Fatalf("body = %q", body)
	}
	if destinations := relay.seen(); len(destinations) == 0 || destinations[0] != api.Listener.Addr().String() {
		t.Fatalf("relay destinations = %v, expected %s", destinations, api.Listener.Addr())
	}

	hosted := newDevice(true)
	if vlessConfigs := hosted.clientStrategy.VlessConfigs(); len(vlessConfigs) != 0 {
		t.Fatalf("the hosted device took %d VLESS servers from its space", len(vlessConfigs))
	}
	changedSettings := *vlessSettings
	changedSettings.Address = "192.0.2.1"
	if errorId := networkSpace.SetVlessSettings(&changedSettings); errorId != "" {
		t.Fatal(errorId)
	}
	if vlessConfigs := networkSpace.clientStrategy.VlessConfigs(); len(vlessConfigs) != 1 || vlessConfigs[0].Address != "192.0.2.1" {
		t.Fatalf("the space did not take the changed settings: %+v", vlessConfigs)
	}
	if vlessConfigs := hosted.clientStrategy.VlessConfigs(); len(vlessConfigs) != 0 {
		t.Fatalf("the hosted device took %d VLESS servers the space named later", len(vlessConfigs))
	}
	hosted.clientStrategy.SetVlessConfigs(spaceVlessConfigs(vlessSettings))
	if vlessConfigs := hosted.clientStrategy.VlessConfigs(); len(vlessConfigs) != 0 {
		t.Fatalf("the hosted device's strategy took %d VLESS servers set on it", len(vlessConfigs))
	}

	// with no VLESS dialer the only route to the api is gone (the primary
	// proof is the dialer set above)
	seenCount := len(relay.seen())
	if body, err := get(hosted.clientStrategy, 2*time.Second); err == nil {
		t.Fatalf("the hosted device reached the api, which only the VLESS server serves: %q", body)
	}
	if destinations := relay.seen(); len(destinations) != seenCount {
		t.Fatalf("the hosted device dialed the VLESS server: %v", destinations[seenCount:])
	}
}

// The space a cloud host shares among its hosted devices refuses VLESS
// settings with the hosted-incompatible no-op: nothing is saved, the getter
// reports none, and its strategy holds no VLESS dialer, even one added to it
// directly. Any other space takes the same settings.
func TestPlatformNetworkSpaceRefusesVlessSettings(t *testing.T) {
	vlessSettings := &VlessSettings{
		Enabled:  true,
		Address:  "192.0.2.1",
		Port:     443,
		Id:       testVlessId,
		Network:  connect.VlessNetworkTcp,
		Security: connect.VlessSecurityNone,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connectSettings := connect.DefaultConnectSettings()
	connectSettings.Log = connect.NewNoopLogger()
	platformSpace := NewPlatformNetworkSpace(ctx, "test", "test", connectSettings)
	defer platformSpace.close()
	if errorId := platformSpace.SetVlessSettings(vlessSettings); errorId != "" {
		t.Fatalf("error id = %q, expected the hosted-incompatible no-op", errorId)
	}
	if settings := platformSpace.GetVlessSettings(); settings.Enabled || settings.Address != "" {
		t.Fatalf("the platform space saved VLESS settings: %+v", settings)
	}
	if vlessConfigs := platformSpace.clientStrategy.VlessConfigs(); len(vlessConfigs) != 0 {
		t.Fatalf("the platform space dials %d VLESS servers", len(vlessConfigs))
	}
	platformSpace.clientStrategy.SetVlessConfigs(spaceVlessConfigs(vlessSettings))
	if vlessConfigs := platformSpace.clientStrategy.VlessConfigs(); len(vlessConfigs) != 0 {
		t.Fatalf("the platform space's strategy took %d VLESS servers set on it", len(vlessConfigs))
	}

	urlsSpace := NewNetworkSpaceWithUrls(ctx, "https://127.0.0.1:1", "wss://127.0.0.1:1", nil)
	defer urlsSpace.close()
	if errorId := urlsSpace.SetVlessSettings(vlessSettings); errorId != "" {
		t.Fatal(errorId)
	}
	if settings := urlsSpace.GetVlessSettings(); !settings.Enabled || settings.Address != vlessSettings.Address {
		t.Fatalf("a space that hosts no cloud devices did not save the settings: %+v", settings)
	}
	if vlessConfigs := urlsSpace.clientStrategy.VlessConfigs(); len(vlessConfigs) != 1 {
		t.Fatalf("a space that hosts no cloud devices dials %d VLESS servers, expected 1", len(vlessConfigs))
	}
}
