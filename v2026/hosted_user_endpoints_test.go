package sdk

// A hosted (cloud) device never dials a server a user names. Besides the VLESS
// server (vless_hosted_test.go), that is the custom extender, the manual
// extender addresses and imports, and the bootstrap DoH servers of its space,
// and the servers of its own dns resolver settings. These tests cover each
// kind on each path: the hosted device's own strategy, the space a cloud host
// shares among its hosted devices, and the device rpc.

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/rpc"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// A hosted device's strategy takes none of its space's custom extender, which
// overrides discovery on a strategy that has one, and keeps none set on it
// later. The space's own strategy, which a device that is not hosted dials
// through, takes it.
func TestHostedClientStrategyRefusesCustomExtenders(t *testing.T) {
	networkSpaceManager := NewNetworkSpaceManagerNoStorage()
	t.Cleanup(networkSpaceManager.Close)
	customIp := netip.MustParseAddr("198.51.100.7")
	networkSpace := networkSpaceManager.updateNetworkSpace(
		NewNetworkSpaceKey("space.example", "main"),
		func(values *NetworkSpaceValues) {
			values.NetExtender = &NetExtender{Ip: customIp.String(), Secret: "synthetic-secret"}
		},
	)
	if customExtenders := networkSpace.clientStrategy.CustomExtenders(); len(customExtenders) != 1 {
		t.Fatalf("the space dials %d custom extenders, expected 1", len(customExtenders))
	}

	strategy := networkSpace.newHostedClientStrategy(nil)
	defer strategy.Close()
	if customExtenders := strategy.CustomExtenders(); len(customExtenders) != 0 {
		t.Fatalf("the hosted strategy took %d custom extenders from its space", len(customExtenders))
	}
	strategy.SetCustomExtenders(map[netip.Addr]string{customIp: "synthetic-secret"})
	if customExtenders := strategy.CustomExtenders(); len(customExtenders) != 0 {
		t.Fatalf("the hosted strategy took %d custom extenders set on it", len(customExtenders))
	}
}

// A hosted device's strategy never dials a manual address of its space's
// extender directory, the address the space's network client adds for each
// manual extender host (K6), which a strategy otherwise dials unverified. Both
// strategies here are derived from the same space settings, reach the api only
// through an extender and record each dial instead of making it. The one
// without the refusal, as a device that is not hosted has, dials the manual
// address; the hosted one dials nothing.
func TestHostedClientStrategyNeverDialsManualExtenders(t *testing.T) {
	networkSpaceManager := NewNetworkSpaceManagerNoStorage()
	t.Cleanup(networkSpaceManager.Close)
	networkSpace := networkSpaceManager.updateNetworkSpace(
		NewNetworkSpaceKey("space.example", "main"),
		func(values *NetworkSpaceValues) {},
	)
	if networkSpace.extenderDirectory == nil {
		t.Fatal("the space has no extender directory")
	}
	manualIp := netip.MustParseAddr("192.0.2.108")
	networkSpace.extenderDirectory.AddManual(manualIp)
	manualAddress := net.JoinHostPort(manualIp.String(), strconv.Itoa(connect.ExtenderTcpPort))

	dialedAddresses := make(chan string, 1024)
	derivedSettings := networkSpace.clientStrategySettings
	derivedSettings.EnableNormal = false
	derivedSettings.EnableResilient = false
	derivedSettings.AltUrl = ""
	derivedSettings.ConnectSettings.DialContextSettings = &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			select {
			case dialedAddresses <- address:
			default:
			}
			return nil, errors.New("the test dials nothing")
		},
		// the udp carriers open no socket either
		PacketConnFactory: func(ctx context.Context) (net.PacketConn, error) {
			return nil, errors.New("the test opens no socket")
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	get := func(requestCtx context.Context, strategy *connect.ClientStrategy) {
		connect.HttpGetWithStrategyRaw(requestCtx, strategy, "https://api.space.example/hello", "")
	}

	allowing := connect.NewClientStrategy(ctx, networkSpace.derivedClientStrategySettings())
	defer allowing.Close()
	allowingCtx, allowingCancel := context.WithTimeout(ctx, 30*time.Second)
	defer allowingCancel()
	allowingDone := make(chan struct{})
	go func() {
		defer close(allowingDone)
		get(allowingCtx, allowing)
	}()
	select {
	case address := <-dialedAddresses:
		if address != manualAddress {
			t.Fatalf("dialed %s, expected the manual address %s", address, manualAddress)
		}
	case <-allowingDone:
		t.Fatal("a strategy that allows manual extenders did not dial the manual address")
	}
	// the request joins its dials before it returns, so nothing it started
	// lands below
	allowingCancel()
	<-allowingDone
	for drained := false; !drained; {
		select {
		case <-dialedAddresses:
		default:
			drained = true
		}
	}

	hosted := networkSpace.newHostedClientStrategy(nil)
	defer hosted.Close()
	// with no dialer the request ends at its timeout; the primary proof of
	// the refusal is the connect test of the directory draw
	hostedCtx, hostedCancel := context.WithTimeout(ctx, 2*time.Second)
	defer hostedCancel()
	get(hostedCtx, hosted)
	select {
	case address := <-dialedAddresses:
		if address == manualAddress {
			t.Fatalf("the hosted strategy dialed the manual address %s", address)
		}
		t.Fatalf("the hosted strategy dialed %s", address)
	default:
	}
}

// A hosted device's strategy queries only the built-in DoH servers: none of
// its space's bootstrap DoH servers, and none set on it later. The space's own
// strategy, which a device that is not hosted dials through, tries the
// bootstrap servers first.
func TestHostedClientStrategyRefusesBootstrapDohServers(t *testing.T) {
	networkSpaceManager := NewNetworkSpaceManagerNoStorage()
	t.Cleanup(networkSpaceManager.Close)
	namedIpv4 := "https://192.0.2.53/dns-query"
	namedIpv6 := "https://[2001:db8::53]/dns-query"
	networkSpace := networkSpaceManager.updateNetworkSpace(
		NewNetworkSpaceKey("space.example", "main"),
		func(values *NetworkSpaceValues) {
			values.ControlDohUrlsIpv4 = []string{namedIpv4}
			values.ControlDohUrlsIpv6 = []string{namedIpv6}
		},
	)
	namesNamedServer := func(settings *connect.DohSettings) bool {
		resolverSettings := settings.DnsResolverSettings
		for _, dohUrls := range [][]string{
			resolverSettings.RemoteDohUrlsIpv4,
			resolverSettings.RemoteDohUrlsIpv6,
			resolverSettings.LocalDohUrlsIpv4,
			resolverSettings.LocalDohUrlsIpv6,
		} {
			if slices.Contains(dohUrls, namedIpv4) || slices.Contains(dohUrls, namedIpv6) {
				return true
			}
		}
		return 0 < len(settings.ServerStatsSeed)
	}
	if spaceDohSettings := networkSpace.clientStrategy.DohSettings(); !namesNamedServer(spaceDohSettings) ||
		spaceDohSettings.DnsResolverSettings.RemoteDohUrlsIpv4[0] != namedIpv4 {
		t.Fatal("the space does not try its bootstrap DoH servers first")
	}

	strategy := networkSpace.newHostedClientStrategy(nil)
	defer strategy.Close()
	builtInSettings := connect.DefaultDnsResolverSettings()
	hostedDohSettings := strategy.DohSettings()
	if namesNamedServer(hostedDohSettings) {
		t.Fatal("the hosted strategy took its space's bootstrap DoH servers")
	}
	if !slices.Equal(hostedDohSettings.DnsResolverSettings.RemoteDohUrlsIpv4, builtInSettings.RemoteDohUrlsIpv4) ||
		!slices.Equal(hostedDohSettings.DnsResolverSettings.RemoteDohUrlsIpv6, builtInSettings.RemoteDohUrlsIpv6) {
		t.Fatal("the hosted strategy does not query the built-in DoH servers")
	}
	strategy.SetInternalDohSettings(connect.ControlDohSettings([]string{namedIpv4}, []string{namedIpv6}))
	if namesNamedServer(strategy.DohSettings()) {
		t.Fatal("the hosted strategy took bootstrap DoH servers set on it")
	}
}

// The space a cloud host shares among its hosted devices refuses every
// setting that names an endpoint, with the hosted-incompatible no-op: the
// bootstrap DoH servers, the extender settings (dns name, gossip url, manual
// hosts) and an import of a share, which would add its addresses and settings.
// Nothing is saved or applied, and the getters report the built-in values.
// Its own strategy refuses a custom extender and a bootstrap DoH server set on
// it directly. An ordinary space takes the same import
// (TestExtenderViewControllerImportsOwnShare).
func TestPlatformNetworkSpaceRefusesUserEndpointSettings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connectSettings := connect.DefaultConnectSettings()
	connectSettings.Log = connect.NewNoopLogger()
	platformSpace := NewPlatformNetworkSpace(ctx, "main", "space.example", connectSettings)
	defer platformSpace.close()
	if platformSpace.extenderDirectory == nil {
		t.Fatal("the platform space has no extender directory")
	}

	dohUrls := NewStringList()
	dohUrls.Add("https://192.0.2.53/dns-query")
	if errorId := platformSpace.SetControlDohUrls(dohUrls); errorId != "" {
		t.Fatalf("error id = %q, expected the hosted-incompatible no-op", errorId)
	}
	if savedDohUrls := platformSpace.GetControlDohUrls(); savedDohUrls.Len() != 0 {
		t.Fatalf("the platform space saved %d bootstrap DoH servers", savedDohUrls.Len())
	}
	platformSpace.clientStrategy.SetInternalDohSettings(connect.ControlDohSettings(
		[]string{"https://192.0.2.53/dns-query"},
		nil,
	))
	if remoteDohUrls := platformSpace.clientStrategy.DohSettings().DnsResolverSettings.RemoteDohUrlsIpv4; slices.Contains(remoteDohUrls, "https://192.0.2.53/dns-query") {
		t.Fatal("the platform space's strategy took a bootstrap DoH server set on it")
	}
	platformSpace.clientStrategy.SetCustomExtenders(map[netip.Addr]string{
		netip.MustParseAddr("198.51.100.7"): "synthetic-secret",
	})
	if customExtenders := platformSpace.clientStrategy.CustomExtenders(); len(customExtenders) != 0 {
		t.Fatalf("the platform space's strategy took %d custom extenders set on it", len(customExtenders))
	}

	deviceSettings := DefaultDeviceLocalSettings()
	deviceSettings.AllowProvider = false
	deviceSettings.DisableLogging = true
	deviceSettings.HostedIncompatible = true
	device, err := newDeviceLocalWithOverrides(platformSpace, "", "", "", "", NewId(), deviceSettings, connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(device.Close)
	vc := newExtenderViewController(device.ctx, device)
	t.Cleanup(vc.Close)

	hosts := NewStringList()
	hosts.Add("192.0.2.1")
	saved := vc.SetSettings("x.example", "wss://g.example/", hosts)
	if !saved.DnsNameDefault || !saved.GossipUrlDefault || saved.Hosts.Len() != 0 {
		t.Fatalf("the platform space saved extender settings: %+v", saved)
	}
	if extenderHosts := platformSpace.GetExtenderHosts(); extenderHosts.Len() != 0 {
		t.Fatalf("the platform space saved %d manual extender hosts", extenderHosts.Len())
	}
	connect.AssertEqual(t, platformSpace.GetExtenderDnsName(), "extender.space.example")
	connect.AssertEqual(t, platformSpace.GetGossipUrl(), "wss://gossip.space.example")

	// a share an ordinary space of the same network builds, settings and all
	otherVc, otherSpace, _ := testExtenderViewController(t)
	otherSpace.extenderDirectory.AddBootstrap(netip.MustParseAddr("192.0.2.1"), connect.ExtenderSourceDns)
	otherDohUrls := NewStringList()
	otherDohUrls.Add("https://192.0.2.53/dns-query")
	if errorId := otherSpace.SetControlDohUrls(otherDohUrls); errorId != "" {
		t.Fatal(errorId)
	}
	share := otherVc.BuildShare(true)
	if share.Count != 1 || !share.IncludesSettings {
		t.Fatalf("the ordinary space built no share to import: %+v", share)
	}
	result := vc.ImportShare(share.Text, true)
	if !result.Ok || result.Error != "" || result.ImportedCount != 0 {
		t.Fatalf("import = %+v, expected the hosted-incompatible no-op", result)
	}
	if entries := platformSpace.extenderDirectory.Snapshot().Entries; len(entries) != 0 {
		t.Fatalf("the platform space imported %d addresses", len(entries))
	}
	if savedDohUrls := platformSpace.GetControlDohUrls(); savedDohUrls.Len() != 0 {
		t.Fatalf("the platform space imported %d bootstrap DoH servers", savedDohUrls.Len())
	}
	connect.AssertEqual(t, platformSpace.GetExtenderDnsName(), "extender.space.example")
}

// A space built to host cloud devices starts with none of the endpoints a user
// names, whatever values it is handed, and its strategy dials none of them:
// the values keep no VLESS server, custom extender, extender settings or
// bootstrap DoH servers, and the strategy holds no VLESS dialer or custom
// extender and queries only the built-in DoH servers.
func TestHostedIncompatibleSpaceStartsWithoutUserEndpoints(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connectSettings := connect.DefaultConnectSettings()
	connectSettings.Log = connect.NewNoopLogger()
	networkSpace := newNetworkSpaceWithConnectSettings(
		ctx,
		NetworkSpaceKey{HostName: "space.example", EnvName: "main"},
		NetworkSpaceValues{
			NetExposeServerIps:       true,
			NetExposeServerHostNames: true,
			Vless:                    testVlessSettings(),
			NetExtender:              &NetExtender{Ip: "198.51.100.7", Secret: "synthetic-secret"},
			ExtenderDnsName:          "x.example",
			GossipUrl:                "wss://g.example",
			ExtenderHosts:            []string{"192.0.2.1"},
			ControlDohUrlsIpv4:       []string{"https://192.0.2.53/dns-query"},
		},
		"",
		connectSettings,
		true,
	)
	defer networkSpace.close()

	values := networkSpace.valuesCopy()
	if values.Vless != nil || values.NetExtender != nil || values.ExtenderDnsName != "" || values.GossipUrl != "" ||
		0 < len(values.ExtenderHosts) || 0 < len(values.ControlDohUrlsIpv4) {
		t.Fatalf("the space kept endpoints a user names: %+v", values)
	}
	if vlessConfigs := networkSpace.clientStrategy.VlessConfigs(); len(vlessConfigs) != 0 {
		t.Fatalf("the space dials %d VLESS servers", len(vlessConfigs))
	}
	if customExtenders := networkSpace.clientStrategy.CustomExtenders(); len(customExtenders) != 0 {
		t.Fatalf("the space dials %d custom extenders", len(customExtenders))
	}
	if remoteDohUrls := networkSpace.clientStrategy.DohSettings().DnsResolverSettings.RemoteDohUrlsIpv4; !slices.Equal(remoteDohUrls, connect.DefaultDnsResolverSettings().RemoteDohUrlsIpv4) {
		t.Fatalf("the space queries %v, expected the built-in DoH servers", remoteDohUrls)
	}
}

// A hosted device takes no dns resolver settings, whose servers its host would
// query: not from its own setter, and not through a hosted rpc, which refuses
// them even in front of a device that is not hosted. A device that is not
// hosted takes them. (A hosted remote queues none:
// TestDeviceRemoteHostedIncompatibleSettersDoNotChangeState.)
func TestHostedDeviceRefusesDnsResolverSettings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace := NewNetworkSpaceWithUrls(ctx, "https://127.0.0.1:1", "wss://127.0.0.1:1", nil)
	defer networkSpace.close()
	newDevice := func(hostedIncompatible bool) *DeviceLocal {
		settings := DefaultDeviceLocalSettings()
		settings.AllowProvider = false
		settings.DisableLogging = true
		settings.HostedIncompatible = hostedIncompatible
		device, err := newDeviceLocalWithOverrides(networkSpace, "", "", "", "", NewId(), settings, connect.NewId())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(device.Close)
		return device
	}
	namedDohUrl := "https://192.0.2.53/dns-query"
	newNamedSettings := func() *DnsResolverSettings {
		localDohUrlsIpv4 := NewStringList()
		localDohUrlsIpv4.Add(namedDohUrl)
		return &DnsResolverSettings{
			EnableLocalDoh:   true,
			LocalDohUrlsIpv4: localDohUrlsIpv4,
		}
	}
	namesNamedServer := func(settings *DnsResolverSettings) bool {
		return settings != nil && settings.LocalDohUrlsIpv4 != nil && slices.Contains(settings.LocalDohUrlsIpv4.getAll(), namedDohUrl)
	}

	device := newDevice(false)
	device.SetDnsResolverSettings(newNamedSettings())
	if !namesNamedServer(device.GetDnsResolverSettings()) {
		t.Fatal("a device that is not hosted did not take the resolver settings")
	}

	hosted := newDevice(true)
	hosted.SetDnsResolverSettings(newNamedSettings())
	if namesNamedServer(hosted.GetDnsResolverSettings()) {
		t.Fatal("the hosted device took the resolver settings")
	}

	rpcDevice := newDevice(false)
	serverConn, clientConn := net.Pipe()
	serverReverseConn, clientReverseConn := net.Pipe()
	rpcSettings := defaultDeviceRpcSettings()
	rpcSettings.DisableLogging = true
	rpcSettings.DisableHostedIncompatible = true
	server := newDeviceLocalRpc(ctx, serverConn, serverReverseConn, rpcDevice, rpcSettings)
	client := rpc.NewClient(clientConn)
	t.Cleanup(func() {
		client.Close()
		clientReverseConn.Close()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := server.CloseAndWait(closeCtx); err != nil {
			t.Error("the hosted rpc did not join")
		}
	})
	var response RpcVoid
	if err := testingPreferenceRpcCall(
		t,
		client,
		"DeviceLocalRpc.SetDnsResolverSettings",
		&DeviceRemoteDnsResolverSettings{DnsResolverSettings: newDnsResolverSettingsRpc(newNamedSettings())},
		&response,
	); err != nil {
		t.Fatal(err)
	}
	if namesNamedServer(rpcDevice.GetDnsResolverSettings()) {
		t.Fatal("the hosted rpc applied the resolver settings")
	}
}

// No device rpc carries a VLESS server, an extender configured by hand or a
// bootstrap DoH server to a hosted device: no argument of a `DeviceLocalRpc`
// method, the sync state included, holds a type that configures one. An rpc
// that carries one must be added as hosted incompatible, at the rpc layer
// (`hostedIncompatibleRpcGuarded`) and in the device, and then allowed here.
// Only exported fields are walked, since gob sends nothing else; an interface
// field cannot be checked statically. The device's dns resolver settings do
// cross the rpc, and are hosted incompatible
// (TestHostedDeviceRefusesDnsResolverSettings).
func TestHostedDeviceRpcCarriesNoUserEndpoints(t *testing.T) {
	userEndpointTypes := map[reflect.Type]bool{
		reflect.TypeOf(VlessSettings{}):                  true,
		reflect.TypeOf(NetExtender{}):                    true,
		reflect.TypeOf(ExtenderSettings{}):               true,
		reflect.TypeOf(NetworkSpaceValues{}):             true,
		reflect.TypeOf(connect.VlessConfig{}):            true,
		reflect.TypeOf(connect.ExtenderConfig{}):         true,
		reflect.TypeOf(connect.DohSettings{}):            true,
		reflect.TypeOf(connect.ClientStrategySettings{}): true,
		reflect.TypeOf(protocol.ExtenderShare{}):         true,
	}
	// walks one type, with the types on the current path to stop at a cycle
	var carriesUserEndpoint func(valueType reflect.Type, pathTypes map[reflect.Type]bool) bool
	carriesUserEndpoint = func(valueType reflect.Type, pathTypes map[reflect.Type]bool) bool {
		if userEndpointTypes[valueType] {
			return true
		}
		if pathTypes[valueType] {
			return false
		}
		pathTypes[valueType] = true
		defer delete(pathTypes, valueType)
		switch valueType.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
			return carriesUserEndpoint(valueType.Elem(), pathTypes)
		case reflect.Map:
			return carriesUserEndpoint(valueType.Key(), pathTypes) || carriesUserEndpoint(valueType.Elem(), pathTypes)
		case reflect.Struct:
			for i := 0; i < valueType.NumField(); i += 1 {
				field := valueType.Field(i)
				if field.IsExported() && carriesUserEndpoint(field.Type, pathTypes) {
					return true
				}
			}
		}
		return false
	}

	// the walk finds an endpoint inside a struct, so a pass is not vacuous
	if !carriesUserEndpoint(reflect.TypeOf(&ExportNetworkSpace{}), map[reflect.Type]bool{}) {
		t.Fatal("the walk misses the endpoints inside a network space export")
	}

	rpcType := reflect.TypeOf(&DeviceLocalRpc{})
	walkedMethodNames := map[string]bool{}
	for i := 0; i < rpcType.NumMethod(); i += 1 {
		method := rpcType.Method(i)
		// the net/rpc method shape: receiver, argument, reply; an error
		if method.Type.NumIn() != 3 || method.Type.NumOut() != 1 {
			continue
		}
		walkedMethodNames[method.Name] = true
		if argumentType := method.Type.In(1); carriesUserEndpoint(argumentType, map[reflect.Type]bool{}) {
			t.Errorf("DeviceLocalRpc.%s carries a user-named endpoint in its %s argument", method.Name, argumentType)
		}
	}
	for _, methodName := range []string{"Sync", "SetTransportSettings", "SetPerformanceProfile", "SetDnsResolverSettings"} {
		if !walkedMethodNames[methodName] {
			t.Errorf("the walk did not reach DeviceLocalRpc.%s", methodName)
		}
	}
}
