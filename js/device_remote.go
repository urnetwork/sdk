//go:build js

package main

import (
	"syscall/js"

	"github.com/urnetwork/sdk"
)

// This file binds the DeviceRemote surface to JS. The DeviceRemote reaches a
// hosted DeviceLocal by connecting directly to the proxy host websocket
// (wss://<proxyHost>/device-rpc, authenticated with the device's signed proxy
// id) and is the JS client's handle to control the hosted device and receive
// its events.
//
// Conventions:
//   - getters return JS values (bool/string/number/object) or null
//   - setters take a single JS argument and return null
//   - listener adders take a JS callback and return an unsubscribe function
//   - the hosted-incompatible setters (route local, provide settings) are
//     accepted here but no-op on the hosted device by design; the getters and
//     listeners still reflect the real device state

// jsSub wraps an sdk.Sub as a JS unsubscribe function.
func jsSub(sub sdk.Sub) js.Value {
	return js.FuncOf(func(this js.Value, args []js.Value) any {
		sub.Close()
		return js.Null()
	}).Value
}

// funcArg returns the first argument if it is a function, else null-safe.
func funcArg(args []js.Value) (js.Value, bool) {
	if len(args) == 0 {
		return js.Null(), false
	}
	if args[0].Type() != js.TypeFunction {
		return js.Null(), false
	}
	return args[0], true
}

func jsConnectLocation(location *sdk.ConnectLocation) js.Value {
	if location == nil {
		return js.Null()
	}
	m := map[string]any{
		"name":          location.Name,
		"locationType":  string(location.LocationType),
		"countryCode":   location.CountryCode,
		"providerCount": location.ProviderCount,
	}
	if location.ConnectLocationId != nil {
		m["connectLocationId"] = location.ConnectLocationId.String()
	}
	return js.ValueOf(m)
}

func jsNetworkPeers(networkPeers *sdk.NetworkPeers) js.Value {
	if networkPeers == nil {
		return js.Null()
	}
	connected := []any{}
	if networkPeers.Connected != nil {
		for i := 0; i < networkPeers.Connected.Len(); i += 1 {
			peer := networkPeers.Connected.Get(i)
			roles := []any{}
			if peer.Roles != nil {
				for j := 0; j < peer.Roles.Len(); j += 1 {
					roles = append(roles, peer.Roles.Get(j))
				}
			}
			m := map[string]any{
				"provideEnabled": peer.ProvideEnabled,
				"principal":      peer.Principal,
				"deviceName":     peer.DeviceName,
				"deviceSpec":     peer.DeviceSpec,
				"roles":          roles,
			}
			if peer.ClientId != nil {
				m["clientId"] = peer.ClientId.String()
			}
			connected = append(connected, m)
		}
	}
	return js.ValueOf(map[string]any{
		"connected":         connected,
		"disconnectedCount": networkPeers.DisconnectedCount,
	})
}

// jsDeviceRemote binds the DeviceRemote surface. See the file header for the
// binding conventions.
func jsDeviceRemote(device *sdk.DeviceRemote) js.Value {
	if device == nil {
		return js.Null()
	}

	m := map[string]any{}

	// lifecycle
	m["close"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		device.Close()
		return js.Null()
	})
	m["cancel"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		device.Cancel()
		return js.Null()
	})
	m["getRemoteConnected"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(device.GetRemoteConnected())
	})

	// offline
	m["getOffline"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(device.GetOffline())
	})
	m["setOffline"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		device.SetOffline(args[0].Bool())
		return js.Null()
	})

	// vpn interface while offline
	m["getVpnInterfaceWhileOffline"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(device.GetVpnInterfaceWhileOffline())
	})
	m["setVpnInterfaceWhileOffline"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		device.SetVpnInterfaceWhileOffline(args[0].Bool())
		return js.Null()
	})

	// route local (hosted-incompatible: no-op on the hosted device)
	m["getRouteLocal"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(device.GetRouteLocal())
	})
	m["setRouteLocal"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		device.SetRouteLocal(args[0].Bool())
		return js.Null()
	})

	// ad/tracker blocker
	m["getBlockerEnabled"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(device.GetBlockerEnabled())
	})
	m["setBlockerEnabled"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		device.SetBlockerEnabled(args[0].Bool())
		return js.Null()
	})

	// provide paused (hosted-incompatible)
	m["getProvidePaused"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(device.GetProvidePaused())
	})
	m["setProvidePaused"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		device.SetProvidePaused(args[0].Bool())
		return js.Null()
	})

	// connect location / destination
	m["getConnectLocation"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsConnectLocation(device.GetConnectLocation())
	})
	m["setConnectLocation"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		device.SetConnectLocation(parseConnectLocation(args[0]))
		return js.Null()
	})
	m["removeDestination"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		device.RemoveDestination()
		return js.Null()
	})
	m["shuffle"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		device.Shuffle()
		return js.Null()
	})

	// tunnel
	m["getTunnelStarted"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(device.GetTunnelStarted())
	})

	// connect state
	m["getConnectEnabled"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(device.GetConnectEnabled())
	})
	m["getProvideEnabled"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(device.GetProvideEnabled())
	})

	// network peers
	m["getNetworkPeers"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsNetworkPeers(device.GetNetworkPeers())
	})

	// custom DNS resolver settings (over the device-rpc)
	m["getDnsResolverSettings"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsDnsResolverSettings(device.GetDnsResolverSettings())
	})
	m["setDnsResolverSettings"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		if 0 < len(args) {
			device.SetDnsResolverSettings(parseDnsResolverSettings(args[0]))
		}
		return js.Null()
	})
	m["addDnsResolverSettingsChangeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddDnsResolverSettingsChangeListener(&jsDnsResolverSettingsChangeListener{cb}))
	})
	// the default (most secure) settings, for a reset action
	m["getDefaultDnsResolverSettings"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsDnsResolverSettings(sdk.GetDefaultDnsResolverSettings())
	})

	// view controllers — the same layer the native app screens are built on
	// (viewControllerManager is embedded in the device). The caller owns the
	// returned vc and must close() it; see view_controllers.go.
	m["openConnectViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsConnectViewController(device.OpenConnectViewController())
	})
	// Deprecated combined controller retained for runtime/declaration and
	// mixed-version compatibility.
	m["openContractDetailsViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsContractDetailsViewController(device.OpenContractDetailsViewController())
	})
	m["openClientContractDetailsViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsContractDetailsViewController(device.OpenClientContractDetailsViewController())
	})
	m["openProviderContractDetailsViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsContractDetailsViewController(device.OpenProviderContractDetailsViewController())
	})
	m["openContractViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsContractViewController(device.OpenContractViewController())
	})
	m["openBlockActionViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsBlockActionViewController(device.OpenBlockActionViewController())
	})
	m["openLocationsViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsLocationsViewController(device.OpenLocationsViewController())
	})
	m["openDevicesViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsDevicesViewController(device.OpenDevicesViewController())
	})

	// listeners
	m["addRemoteChangeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddRemoteChangeListener(&jsRemoteChangeListener{cb}))
	})
	m["addDeviceRecreatedListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddDeviceRecreatedListener(&jsDeviceRecreatedListener{cb}))
	})
	m["addConnectChangeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddConnectChangeListener(&jsConnectChangeListener{cb}))
	})
	m["addOfflineChangeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddOfflineChangeListener(&jsOfflineChangeListener{cb}))
	})
	m["addConnectLocationChangeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddConnectLocationChangeListener(&jsConnectLocationChangeListener{cb}))
	})
	m["addNetworkPeersChangeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddNetworkPeersChangeListener(&jsNetworkPeersChangeListener{cb}))
	})

	return js.ValueOf(m)
}

// listener adapters

type jsRemoteChangeListener struct{ cb js.Value }

func (self *jsRemoteChangeListener) RemoteChanged(remoteConnected bool) {
	self.cb.Invoke(remoteConnected)
}

type jsDeviceRecreatedListener struct{ cb js.Value }

func (self *jsDeviceRecreatedListener) DeviceRecreated() {
	self.cb.Invoke()
}

type jsConnectChangeListener struct{ cb js.Value }

func (self *jsConnectChangeListener) ConnectChanged(connectEnabled bool) {
	self.cb.Invoke(connectEnabled)
}

type jsOfflineChangeListener struct{ cb js.Value }

func (self *jsOfflineChangeListener) OfflineChanged(offline bool, vpnInterfaceWhileOffline bool) {
	self.cb.Invoke(offline, vpnInterfaceWhileOffline)
}

type jsConnectLocationChangeListener struct{ cb js.Value }

func (self *jsConnectLocationChangeListener) ConnectLocationChanged(location *sdk.ConnectLocation) {
	self.cb.Invoke(jsConnectLocation(location))
}

type jsNetworkPeersChangeListener struct{ cb js.Value }

func (self *jsNetworkPeersChangeListener) NetworkPeersChanged(networkPeers *sdk.NetworkPeers) {
	self.cb.Invoke(jsNetworkPeers(networkPeers))
}

// ── DNS resolver settings ────────────────────────────────────────────────────

func jsStringListDR(list *sdk.StringList) js.Value {
	out := []any{}
	if list != nil {
		for i := 0; i < list.Len(); i += 1 {
			out = append(out, list.Get(i))
		}
	}
	return js.ValueOf(out)
}

func parseStringListDR(v js.Value) *sdk.StringList {
	list := sdk.NewStringList()
	if v.Type() == js.TypeObject && v.Get("length").Type() == js.TypeNumber {
		n := v.Get("length").Int()
		for i := 0; i < n; i += 1 {
			item := v.Index(i)
			if item.Type() == js.TypeString {
				list.Add(item.String())
			}
		}
	}
	return list
}

// jsDnsResolverSettings mirrors the DoH/DNS toggles + per-family server lists the
// native DNS editor renders.
func jsDnsResolverSettings(s *sdk.DnsResolverSettings) js.Value {
	if s == nil {
		return js.Null()
	}
	return js.ValueOf(map[string]any{
		"enableRemoteDoh": s.EnableRemoteDoh,
		"enableLocalDoh":  s.EnableLocalDoh,
		"enableRemoteDns": s.EnableRemoteDns,
		"enableLocalDns":  s.EnableLocalDns,
		"enableFallback":  s.EnableFallback,

		"remoteDohUrlsIpv4": jsStringListDR(s.RemoteDohUrlsIpv4),
		"remoteDohUrlsIpv6": jsStringListDR(s.RemoteDohUrlsIpv6),
		"localDohUrlsIpv4":  jsStringListDR(s.LocalDohUrlsIpv4),
		"localDohUrlsIpv6":  jsStringListDR(s.LocalDohUrlsIpv6),
		"remoteDnsIpv4":     jsStringListDR(s.RemoteDnsIpv4),
		"remoteDnsIpv6":     jsStringListDR(s.RemoteDnsIpv6),
		"localDnsIpv4":      jsStringListDR(s.LocalDnsIpv4),
		"localDnsIpv6":      jsStringListDR(s.LocalDnsIpv6),
	})
}

func parseDnsResolverSettings(v js.Value) *sdk.DnsResolverSettings {
	if v.IsNull() || v.IsUndefined() {
		return sdk.GetDefaultDnsResolverSettings()
	}
	b := func(key string) bool {
		x := v.Get(key)
		return x.Type() == js.TypeBoolean && x.Bool()
	}
	return &sdk.DnsResolverSettings{
		EnableRemoteDoh: b("enableRemoteDoh"),
		EnableLocalDoh:  b("enableLocalDoh"),
		EnableRemoteDns: b("enableRemoteDns"),
		EnableLocalDns:  b("enableLocalDns"),
		EnableFallback:  b("enableFallback"),

		RemoteDohUrlsIpv4: parseStringListDR(v.Get("remoteDohUrlsIpv4")),
		RemoteDohUrlsIpv6: parseStringListDR(v.Get("remoteDohUrlsIpv6")),
		LocalDohUrlsIpv4:  parseStringListDR(v.Get("localDohUrlsIpv4")),
		LocalDohUrlsIpv6:  parseStringListDR(v.Get("localDohUrlsIpv6")),
		RemoteDnsIpv4:     parseStringListDR(v.Get("remoteDnsIpv4")),
		RemoteDnsIpv6:     parseStringListDR(v.Get("remoteDnsIpv6")),
		LocalDnsIpv4:      parseStringListDR(v.Get("localDnsIpv4")),
		LocalDnsIpv6:      parseStringListDR(v.Get("localDnsIpv6")),
	}
}

type jsDnsResolverSettingsChangeListener struct{ cb js.Value }

func (self *jsDnsResolverSettingsChangeListener) DnsResolverSettingsChanged(s *sdk.DnsResolverSettings) {
	self.cb.Invoke(jsDnsResolverSettings(s))
}

// parseConnectLocation builds a ConnectLocation from a JS object with a
// connectLocationId string (or bestAvailable true).
func parseConnectLocation(v js.Value) *sdk.ConnectLocation {
	if v.IsNull() || v.IsUndefined() {
		return nil
	}
	location := &sdk.ConnectLocation{}
	if idv := v.Get("connectLocationId"); idv.Type() == js.TypeString {
		if id, err := parseConnectLocationId(idv.String()); err == nil {
			location.ConnectLocationId = id
		}
	}
	if bv := v.Get("bestAvailable"); bv.Type() == js.TypeBoolean && bv.Bool() {
		location.ConnectLocationId = &sdk.ConnectLocationId{BestAvailable: true}
	}
	if nv := v.Get("name"); nv.Type() == js.TypeString {
		location.Name = nv.String()
	}
	return location
}

func parseConnectLocationId(s string) (*sdk.ConnectLocationId, error) {
	id, err := sdk.ParseId(s)
	if err != nil {
		return nil, err
	}
	return &sdk.ConnectLocationId{LocationId: id}, nil
}

// NewPlatformDeviceRemote(apiUrl, platformUrl, byJwt, proxyUrl, signedProxyId)
// builds a DeviceRemote that controls a hosted DeviceLocal by connecting
// directly to the proxy host at wss://<proxyUrl>/device-rpc, authenticating
// with the device's signed proxy id (not a jwt). byJwt is the network member
// jwt for the network space api.
func NewPlatformDeviceRemote(this js.Value, args []js.Value) any {
	if len(args) < 5 {
		return js.Null()
	}
	apiUrl := args[0].String()
	platformUrl := args[1].String()
	byJwt := args[2].String()
	proxyUrl := args[3].String()
	signedProxyId := args[4].String()

	networkSpace := sdk.NewUrlsNetworkSpace(apiUrl, platformUrl)

	instanceId := sdk.NewId()
	device, err := sdk.NewPlatformDeviceRemote(networkSpace, byJwt, proxyUrl, signedProxyId, instanceId)
	if err != nil {
		return js.ValueOf(map[string]any{"error": err.Error()})
	}
	return jsDeviceRemote(device)
}
