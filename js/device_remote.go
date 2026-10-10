//go:build js

package main

import (
	"context"
	"encoding/json"
	"strings"
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
//   - the hosted-incompatible setters (route local, provide settings, dns
//     resolver settings) are accepted here but no-op on the hosted device by
//     design; the getters and listeners still reflect the real device state

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
		// the dot color, from the sdk palette (no "#"); "" for best available
		"colorHex": location.ColorHex(),
	}
	if location.ConnectLocationId != nil {
		// the composite id, the one to hand back to the sdk
		m["connectLocationId"] = location.ConnectLocationId.String()
		// ...and its parts, for callers that persist or forward a bare id
		// (the web settings doc and the extension both carry a location id,
		// not the composite). Exactly one of these is set.
		if location.ConnectLocationId.LocationId != nil {
			m["locationId"] = location.ConnectLocationId.LocationId.String()
		}
		if location.ConnectLocationId.LocationGroupId != nil {
			m["locationGroupId"] = location.ConnectLocationId.LocationGroupId.String()
		}
		if location.ConnectLocationId.ClientId != nil {
			m["clientId"] = location.ConnectLocationId.ClientId.String()
		}
		m["bestAvailable"] = location.ConnectLocationId.BestAvailable
	}
	return js.ValueOf(m)
}

func jsNetworkPeers(networkPeers *sdk.NetworkPeers) js.Value {
	if networkPeers == nil {
		return js.Null()
	}
	return js.ValueOf(map[string]any{
		"connected":         jsNetworkPeerList(networkPeers.Connected),
		"disconnectedCount": networkPeers.DisconnectedCount,
	})
}

// jsConnectedProviderLocation marshals one connected provider. The flags are
// carried through rather than collapsed to nulls: `hasLocation` false is a
// real state (the user's own fixed peers and restored window identities never
// have one), and 0,0 is a valid coordinate.
func jsConnectedProviderLocation(location *sdk.ConnectedProviderLocation) js.Value {
	if location == nil {
		return js.Null()
	}
	m := map[string]any{
		"country":              location.Country,
		"countryCode":          location.CountryCode,
		"region":               location.Region,
		"city":                 location.City,
		"regionLat":            location.RegionLat,
		"regionLon":            location.RegionLon,
		"cityLat":              location.CityLat,
		"cityLon":              location.CityLon,
		"hasLocation":          location.HasLocation,
		"hasRegionCoordinates": location.HasRegionCoordinates,
		"hasCityCoordinates":   location.HasCityCoordinates,
		"connectedSinceMillis": location.ConnectedSinceMillis,
		// the address-family category ("dualstack" | "v4-only" | "v6-only")
		// and its display label ("both" | "v4" | "v6") for the provider rows
		"ipFamily":      location.IpFamily,
		"ipFamilyLabel": location.IpFamilyLabel,
		// the dot color from the sdk palette: the country's when the location
		// is known, else the stable per-client color
		"colorHex": location.ColorHex(),
	}
	if location.ClientId != nil {
		m["clientId"] = location.ClientId.String()
	}
	return js.ValueOf(m)
}

// jsConnectedProviderLocations marshals the list, preserving whatever order it
// arrives in — the sdk's own (oldest connected first) from the device, display
// order from `ProviderLocationsViewController.getProviderLocations`.
func jsConnectedProviderLocations(locations *sdk.ConnectedProviderLocationList) js.Value {
	out := []any{}
	if locations != nil {
		for i := 0; i < locations.Len(); i += 1 {
			out = append(out, jsConnectedProviderLocation(locations.Get(i)))
		}
	}
	return js.ValueOf(out)
}

// jsDeviceRemote binds the DeviceRemote surface. See the file header for the
// binding conventions.
func jsDeviceRemote(device *sdk.DeviceRemote) js.Value {
	if device == nil {
		return js.Null()
	}

	m := map[string]any{}
	socketHandles := jsBindSocketDevice(device, m)
	subprotocolHandles := jsBindSubprotocolDevice(device.Ctx(), func(ctx context.Context, id int32) (jsSubprotocol, error) {
		return device.OpenSubprotocolContext(ctx, id)
	}, m)

	// lifecycle
	m["close"] = jsViewControllerClose(device.Close, socketHandles.close, subprotocolHandles.close)
	m["cancel"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		go socketHandles.close()
		go subprotocolHandles.close()
		device.Cancel()
		return js.Null()
	})
	m["getRemoteConnected"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(device.GetRemoteConnected())
	})
	m["getSyncError"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(device.GetSyncError())
	})
	// getAuthLogoutCause(): why the server ended the device's sign-in once its
	// logout fired, "session_revoked" (signed out from another device) or ""
	m["getAuthLogoutCause"] = js.FuncOf(func(js.Value, []js.Value) any {
		return js.ValueOf(device.GetAuthLogoutCause())
	})
	m["getClientId"] = js.FuncOf(func(js.Value, []js.Value) any { return device.GetClientId().String() })
	m["getLicenses"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsLicenses(device.GetLicenses(stringArg(args, 0)))
	})
	m["getInstanceId"] = js.FuncOf(func(js.Value, []js.Value) any { return device.GetInstanceId().String() })
	// suggestEmojiTag(count): synchronous; a random tag of 1–3 distinct emoji
	// to prefill the emoji-tag editor with (count 0 or omitted picks the
	// length at random). Pure; no device state involved.
	m["suggestEmojiTag"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(sdk.SuggestEmojiTag(int(int64Arg(args, 0))))
	})

	// offline
	m["getOffline"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(device.GetOffline())
	})
	m["setOffline"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		device.SetOffline(args[0].Bool())
		device.Sync()
		return js.Null()
	})

	// vpn interface while offline
	m["getVpnInterfaceWhileOffline"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(device.GetVpnInterfaceWhileOffline())
	})
	m["setVpnInterfaceWhileOffline"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		device.SetVpnInterfaceWhileOffline(args[0].Bool())
		device.Sync()
		return js.Null()
	})

	// route local (hosted-incompatible: no-op on the hosted device)
	m["getRouteLocal"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(device.GetRouteLocal())
	})
	m["setRouteLocal"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		device.SetRouteLocal(args[0].Bool())
		device.Sync()
		return js.Null()
	})

	// ad/tracker blocker
	m["getBlockerEnabled"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(device.GetBlockerEnabled())
	})
	m["setBlockerEnabled"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		device.SetBlockerEnabled(args[0].Bool())
		device.Sync()
		return js.Null()
	})

	// provide paused (hosted-incompatible)
	m["getProvidePaused"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(device.GetProvidePaused())
	})
	m["setProvidePaused"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		device.SetProvidePaused(args[0].Bool())
		device.Sync()
		return js.Null()
	})
	m["addProvidePausedChangeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddProvidePausedChangeListener(&jsProvidePausedChangeListener{cb: cb}))
	})

	// provide mode (hosted-incompatible setter): 0 none, 1 network, 2 friends
	// and family, 3 public
	m["getProvideMode"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(device.GetProvideMode())
	})
	m["setProvideMode"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		if len(args) == 0 || args[0].Type() != js.TypeNumber {
			return js.Null()
		}
		device.SetProvideMode(args[0].Int())
		device.Sync()
		return js.Null()
	})
	m["addProvideModeChangeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddProvideModeChangeListener(&jsProvideModeChangeListener{cb: cb}))
	})
	m["addProvideChangeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddProvideChangeListener(&jsProvideChangeListener{cb: cb}))
	})

	// the provider of the device process: whether it has a platform transport
	// with a registered route, the platform's client limit hold, and the
	// traffic it relays. The getters read the provider state the device
	// pushes to the remote after every change, so they need no listener to
	// stay current
	m["getProviderConnected"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(device.GetProviderConnected())
	})
	m["getClientLimitStatus"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsClientLimitStatus(device.GetClientLimitStatus())
	})
	m["addClientLimitStatusChangeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddClientLimitStatusChangeListener(&jsClientLimitStatusChangeListener{cb: cb}))
	})
	m["getProviderPacketStats"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsPacketStats(device.GetProviderPacketStats())
	})
	m["addProviderPacketStatsChangeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddProviderPacketStatsChangeListener(&jsPacketStatsChangeListener{cb: cb}))
	})
	m["getProviderEgressContractDetails"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsContractDetailsList(device.GetProviderEgressContractDetails())
	})
	m["getProviderIngressContractDetails"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsContractDetailsList(device.GetProviderIngressContractDetails())
	})
	m["addProviderEgressContractDetailsChangeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddProviderEgressContractDetailsChangeListener(&jsContractDetailsChangeListener{cb: cb}))
	})
	m["addProviderIngressContractDetailsChangeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddProviderIngressContractDetailsChangeListener(&jsContractDetailsChangeListener{cb: cb}))
	})

	// connect location / destination
	m["getConnectLocation"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsConnectLocation(device.GetConnectLocation())
	})
	m["setConnectLocation"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		device.SetConnectLocation(parseConnectLocation(args[0]))
		device.Sync()
		return js.Null()
	})
	// the explicit "connect to this" action: rebuilds even when the location is
	// already the installed destination (see Device.Reconnect)
	m["reconnect"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		device.Reconnect(parseConnectLocation(args[0]))
		device.Sync()
		return js.Null()
	})
	m["removeDestination"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		device.RemoveDestination()
		device.Sync()
		return js.Null()
	})
	m["shuffle"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		device.Shuffle()
		device.Sync()
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

	// connected provider locations. While the rpc is down the device retains
	// the last readable list rather than draining it, so an empty array here
	// is a fact, not a stale zero — pair it with getRemoteConnected.
	m["getConnectedProviderLocations"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsConnectedProviderLocations(device.GetConnectedProviderLocations())
	})
	// drop a provider from the connection and stop it being re-discovered for
	// the rest of this connection. Takes the egress client id as reported by
	// getConnectedProviderLocations
	m["removeConnectedProvider"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		if len(args) == 0 || args[0].Type() != js.TypeString {
			return js.Null()
		}
		clientId, err := sdk.ParseId(args[0].String())
		if err != nil {
			return js.Null()
		}
		device.RemoveConnectedProvider(clientId)
		return js.Null()
	})

	// custom DNS resolver settings (over the device-rpc)
	m["getDnsResolverSettings"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsDnsResolverSettings(device.GetDnsResolverSettings())
	})
	m["setDnsResolverSettings"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		if 0 < len(args) {
			device.SetDnsResolverSettings(parseDnsResolverSettings(args[0]))
			device.Sync()
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

	// performance profile (over the device-rpc): the connect options the
	// native apps write -- the window type, the window size (Fixed IP is a
	// window of exactly one exit), direct mode and post quantum encryption.
	// The device applies a change to the live connection, as it does for the
	// native apps. A hosted device forces allowDirect off (see
	// sdk.DeviceLocal.hostedSafePerformanceProfile); the getter and the
	// listener report what the device holds
	m["getPerformanceProfile"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsPerformanceProfile(device.GetPerformanceProfile())
	})
	m["setPerformanceProfile"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		if 0 < len(args) {
			if performanceProfile, ok := parsePerformanceProfile(args[0]); ok {
				device.SetPerformanceProfile(performanceProfile)
				device.Sync()
			}
		}
		return js.Null()
	})
	m["addPerformanceProfileChangeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddPerformanceProfileChangeListener(&jsPerformanceProfileChangeListener{cb: cb}))
	})

	// transport policy (over the device-rpc): one carrier or Auto over the
	// enabled carriers. see sdk.TransportSettings. A hosted device (the web's
	// cloud proxy) is pinned to h1 and ignores the setters; the getters and
	// listeners still work, so the policy can be shown
	m["getTransportSettings"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsTransportSettings(device.GetTransportSettings())
	})
	m["getProviderTransportSettings"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsTransportSettings(device.GetProviderTransportSettings())
	})
	m["getTransportStatus"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsTransportStatus(device.GetTransportStatus())
	})
	m["getProviderTransportStatus"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsTransportStatus(device.GetProviderTransportStatus())
	})
	m["getProviderFamilyTransportStatus"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsProviderFamilyTransportStatus(device.GetProviderFamilyTransportStatus())
	})
	m["setTransportSettings"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		if 0 < len(args) {
			device.SetTransportSettings(parseTransportSettings(args[0]))
			device.Sync()
		}
		return js.Null()
	})
	m["setProviderTransportSettings"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		if 0 < len(args) {
			device.SetProviderTransportSettings(parseTransportSettings(args[0]))
			device.Sync()
		}
		return js.Null()
	})
	m["addTransportSettingsChangeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddTransportSettingsChangeListener(&jsTransportSettingsChangeListener{cb}))
	})
	m["addProviderTransportSettingsChangeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddProviderTransportSettingsChangeListener(&jsProviderTransportSettingsChangeListener{cb}))
	})
	m["addTransportStatusChangeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddTransportStatusChangeListener(&jsTransportStatusChangeListener{cb}))
	})
	m["addProviderTransportStatusChangeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddProviderTransportStatusChangeListener(&jsProviderTransportStatusChangeListener{cb}))
	})
	m["getDefaultTransportSettings"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsTransportSettings(sdk.DefaultTransportSettings())
	})
	m["getDefaultProviderTransportSettings"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsTransportSettings(sdk.DefaultProviderTransportSettings())
	})
	// the selectable modes in default preference order (h1, h3, dns, dnspump)
	m["getSelectableTransportModes"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsStringListDR(sdk.SelectableTransportModes())
	})
	// the shared editing rules over a policy value: an edited copy (a refused
	// edit -- disabling the last Auto mode -- returns an equal copy)
	m["transportSettingsWithMode"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		if len(args) < 2 || args[1].Type() != js.TypeString {
			return js.Null()
		}
		return jsTransportSettings(sdk.TransportSettingsWithMode(parseTransportSettings(args[0]), args[1].String()))
	})
	m["transportSettingsWithAutoModeEnabled"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		if len(args) < 3 || args[1].Type() != js.TypeString || args[2].Type() != js.TypeBoolean {
			return js.Null()
		}
		return jsTransportSettings(sdk.TransportSettingsWithAutoModeEnabled(parseTransportSettings(args[0]), args[1].String(), args[2].Bool()))
	})
	m["transportSettingsEqual"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		if len(args) < 2 {
			return js.ValueOf(false)
		}
		return js.ValueOf(sdk.TransportSettingsEqual(parseTransportSettings(args[0]), parseTransportSettings(args[1])))
	})

	// view controllers — the same layer the native app screens are built on
	// (viewControllerManager is embedded in the device). The caller owns the
	// returned vc and must close() it; see view_controllers.go.
	m["openConnectViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc := device.OpenConnectViewController()
		return jsConnectViewController(vc, func() {
			device.CloseConnectViewController(vc)
		})
	})
	// Deprecated combined controller retained for runtime/declaration and
	// mixed-version compatibility.
	m["openContractDetailsViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc := device.OpenContractDetailsViewController()
		return jsContractDetailsViewController(vc, func() {
			device.CloseContractDetailsViewController(vc)
		})
	})
	m["openClientContractDetailsViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc := device.OpenClientContractDetailsViewController()
		return jsContractDetailsViewController(vc, func() {
			device.CloseContractDetailsViewController(vc)
		})
	})
	m["openProviderContractDetailsViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc := device.OpenProviderContractDetailsViewController()
		return jsContractDetailsViewController(vc, func() {
			device.CloseContractDetailsViewController(vc)
		})
	})
	m["openContractViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc := device.OpenContractViewController()
		return jsContractViewController(vc, func() {
			device.CloseContractViewController(vc)
		})
	})
	m["openBlockActionViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc := device.OpenBlockActionViewController()
		return jsBlockActionViewController(vc, func() {
			device.CloseBlockActionViewController(vc)
		})
	})
	m["openLocationsViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc := device.OpenLocationsViewController()
		return jsLocationsViewController(vc, func() {
			device.CloseLocationsViewController(vc)
		})
	})
	m["setClientInfo"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		device.GetApi().SetClientInfo(sdk.NewClientInfo(stringArg(args, 0), stringArg(args, 1)))
		return js.Null()
	})
	m["openClientSessionViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc := device.OpenClientSessionViewController()
		return jsClientSessionViewController(vc, func() { device.CloseClientSessionViewController(vc) })
	})
	m["openDevicesViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc := device.OpenDevicesViewController()
		return jsDevicesViewController(vc, func() {
			device.CloseDevicesViewController(vc)
		})
	})
	m["openPointsLeaderboardViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc := device.OpenPointsLeaderboardViewController()
		return jsPointsLeaderboardViewController(vc, func() {
			device.ClosePointsLeaderboardViewController(vc)
		})
	})
	m["openPeerViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc := device.OpenPeerViewController()
		return jsPeerViewController(vc, func() {
			device.ClosePeerViewController(vc)
		})
	})
	m["openProviderLocationsViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc := device.OpenProviderLocationsViewController()
		return jsProviderLocationsViewController(vc, func() {
			device.CloseProviderLocationsViewController(vc)
		})
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
	// signal only: the device may not hold what the page applied to it (the
	// first sync, a recreated device, or a device without a generation), so
	// the page applies its own settings again. Add it right after creating the
	// remote; the remote's first sync waits for the transport to open, which
	// takes a later JavaScript task
	m["addDeviceConfigurationChangedListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddDeviceConfigurationChangedListener(&jsDeviceConfigurationChangedListener{cb: cb}))
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
	// signal only: the callback takes no arguments and the consumer re-reads
	// getConnectedProviderLocations
	m["addConnectedProviderLocationChangeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(device.AddConnectedProviderLocationChangeListener(
			&jsConnectedProviderLocationChangeListener{cb},
		))
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

// Invokes the page's callback with no arguments; the page re-reads the getters.
type jsDeviceConfigurationChangedListener struct{ cb js.Value }

func (self *jsDeviceConfigurationChangedListener) DeviceConfigurationChanged() {
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

type jsConnectedProviderLocationChangeListener struct{ cb js.Value }

func (self *jsConnectedProviderLocationChangeListener) ConnectedProviderLocationsChanged() {
	self.cb.Invoke()
}

// Calls the page with the provide enabled state.
type jsProvideChangeListener struct{ cb js.Value }

func (self *jsProvideChangeListener) ProvideChanged(provideEnabled bool) {
	self.cb.Invoke(provideEnabled)
}

// Calls the page with the provide paused state.
type jsProvidePausedChangeListener struct{ cb js.Value }

func (self *jsProvidePausedChangeListener) ProvidePausedChanged(providePaused bool) {
	self.cb.Invoke(providePaused)
}

// Calls the page with the provide mode.
type jsProvideModeChangeListener struct{ cb js.Value }

func (self *jsProvideModeChangeListener) ProvideModeChanged(provideMode int) {
	self.cb.Invoke(provideMode)
}

// Calls the page with the client limit status, as jsClientLimitStatus renders it.
type jsClientLimitStatusChangeListener struct{ cb js.Value }

func (self *jsClientLimitStatusChangeListener) ClientLimitStatusChanged(status *sdk.ClientLimitStatus) {
	self.cb.Invoke(jsClientLimitStatus(status))
}

// Calls the page with the packet stats, as jsPacketStats renders them.
type jsPacketStatsChangeListener struct{ cb js.Value }

func (self *jsPacketStatsChangeListener) PacketStatsChanged(packetStats *sdk.PacketStats) {
	self.cb.Invoke(jsPacketStats(packetStats))
}

// Calls the page with one contract row, as jsContractDetails renders it.
type jsContractDetailsChangeListener struct{ cb js.Value }

func (self *jsContractDetailsChangeListener) ContractDetailsChanged(contractDetails *sdk.ContractDetails) {
	self.cb.Invoke(jsContractDetails(contractDetails))
}

// ── provider status ──────────────────────────────────────────────────────────

// Mirrors sdk.ClientLimitStatus ({status, retryTime}):
// status is "" or "client_limit_exceeded", and retryTime the unix millisecond
// time the device reconnects, 0 with no hold
func jsClientLimitStatus(status *sdk.ClientLimitStatus) js.Value {
	if status == nil {
		return js.Null()
	}
	return js.ValueOf(map[string]any{
		"status":    status.Status,
		"retryTime": status.RetryTime,
	})
}

// Mirrors sdk.TransferPath ({sourceId, destinationId,
// streamId}). An id the path does not carry is null; the sdk reports an end it
// does not know, and the stream of a direct contract, as the all-zero id
func jsTransferPath(path *sdk.TransferPath) js.Value {
	if path == nil {
		return js.Null()
	}
	id := func(id *sdk.Id) any {
		if id == nil {
			return nil
		}
		return id.String()
	}
	return js.ValueOf(map[string]any{
		"sourceId":      id(path.SourceId),
		"destinationId": id(path.DestinationId),
		"streamId":      id(path.StreamId),
	})
}

// Mirrors sdk.ContractDetails, one contract of one
// direction: its id, used and total bytes, bit rate, transfer path and status
// ("open", or "closed" once with the final counts)
func jsContractDetails(contractDetails *sdk.ContractDetails) js.Value {
	if contractDetails == nil {
		return js.Null()
	}
	var contractId any
	if contractDetails.ContractId != nil {
		contractId = contractDetails.ContractId.String()
	}
	return js.ValueOf(map[string]any{
		"contractId":            contractId,
		"contractUsedByteCount": contractDetails.ContractUsedByteCount,
		"contractByteCount":     contractDetails.ContractByteCount,
		"contractBitRate":       contractDetails.ContractBitRate,
		"contractTransferPath":  jsTransferPath(contractDetails.ContractTransferPath),
		"status":                contractDetails.Status,
	})
}

// Mirrors a contract details list: null when the device
// has no provider, else an array of rows in no particular order
func jsContractDetailsList(contractDetailsList *sdk.ContractDetailsList) js.Value {
	if contractDetailsList == nil {
		return js.Null()
	}
	rows := []any{}
	for i := 0; i < contractDetailsList.Len(); i += 1 {
		rows = append(rows, jsContractDetails(contractDetailsList.Get(i)))
	}
	return js.ValueOf(rows)
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
		"enableRemoteDoh":       s.EnableRemoteDoh,
		"enableLocalDoh":        s.EnableLocalDoh,
		"enableRemoteDns":       s.EnableRemoteDns,
		"enableLocalDns":        s.EnableLocalDns,
		"enableFallback":        s.EnableFallback,
		"dnsUpgradeMaskAddress": s.DnsUpgradeMaskAddress,

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
	defaults := sdk.GetDefaultDnsResolverSettings()
	dnsUpgradeMaskAddress := ""
	if defaults != nil {
		dnsUpgradeMaskAddress = defaults.DnsUpgradeMaskAddress
	}
	if x := v.Get("dnsUpgradeMaskAddress"); x.Type() == js.TypeString && strings.TrimSpace(x.String()) != "" {
		dnsUpgradeMaskAddress = strings.TrimSpace(x.String())
	}
	return &sdk.DnsResolverSettings{
		EnableRemoteDoh:       b("enableRemoteDoh"),
		EnableLocalDoh:        b("enableLocalDoh"),
		EnableRemoteDns:       b("enableRemoteDns"),
		EnableLocalDns:        b("enableLocalDns"),
		EnableFallback:        b("enableFallback"),
		DnsUpgradeMaskAddress: dnsUpgradeMaskAddress,

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

// jsTransportSettings mirrors the transport policy: the mode ("auto" or a
// carrier), the Auto priority rows, and the derived views every renderer needs
// (the Auto modes in preference order and the carriers the policy enables) so
// the app never re-implements the rules
func jsTransportSettings(s *sdk.TransportSettings) js.Value {
	if s == nil {
		return js.Null()
	}
	priorities := []any{}
	if s.AutoModePriorities != nil {
		for i := 0; i < s.AutoModePriorities.Len(); i += 1 {
			item := s.AutoModePriorities.Get(i)
			if item == nil {
				continue
			}
			priorities = append(priorities, map[string]any{
				"mode":     item.Mode,
				"priority": item.Priority,
			})
		}
	}
	return js.ValueOf(map[string]any{
		"mode":                  s.Mode,
		"autoModePriorities":    priorities,
		"autoModes":             jsStringListDR(s.AutoModes()),
		"enabledTransportTypes": jsStringListDR(s.EnabledTransportTypes()),
	})
}

func jsTransportStatus(status *sdk.TransportStatus) js.Value {
	if status == nil {
		return js.Null()
	}
	return js.ValueOf(map[string]any{
		"autoDegraded":      status.AutoDegraded,
		"autoEligibleModes": jsStringListDR(status.AutoEligibleModes),
		"autoConstraint":    status.AutoConstraint,
	})
}

// jsProviderFamilyTransportStatus is the per-family provider transport
// readout ({hasIpv4, ipv4State, hasIpv6, ipv6State, standbyState,
// standbyActive}); states are the connect transport state strings.
func jsProviderFamilyTransportStatus(status *sdk.ProviderFamilyTransportStatus) js.Value {
	if status == nil {
		return js.Null()
	}
	return js.ValueOf(map[string]any{
		"hasIpv4":       status.HasIpv4,
		"ipv4State":     status.Ipv4State,
		"hasIpv6":       status.HasIpv6,
		"ipv6State":     status.Ipv6State,
		"standbyState":  status.StandbyState,
		"standbyActive": status.StandbyActive,
	})
}

// parseTransportSettings reads a policy from a JS object ({mode,
// autoModePriorities: [{mode, priority}]}); null reads as the default policy.
// The sdk normalizes what it is given
func parseTransportSettings(v js.Value) *sdk.TransportSettings {
	if v.IsNull() || v.IsUndefined() {
		return sdk.DefaultTransportSettings()
	}
	settings := &sdk.TransportSettings{
		Mode:               sdk.TransportModeAuto,
		AutoModePriorities: sdk.NewTransportModePriorityList(),
	}
	if x := v.Get("mode"); x.Type() == js.TypeString {
		settings.Mode = x.String()
	}
	if items := v.Get("autoModePriorities"); items.Type() == js.TypeObject {
		n := items.Length()
		for i := 0; i < n; i += 1 {
			item := items.Index(i)
			mode := item.Get("mode")
			priority := item.Get("priority")
			if mode.Type() != js.TypeString || priority.Type() != js.TypeNumber {
				continue
			}
			settings.AutoModePriorities.Add(&sdk.TransportModePriority{
				Mode:     mode.String(),
				Priority: priority.Int(),
			})
		}
	}
	return settings
}

type jsTransportSettingsChangeListener struct{ cb js.Value }

func (self *jsTransportSettingsChangeListener) TransportSettingsChanged(s *sdk.TransportSettings) {
	self.cb.Invoke(jsTransportSettings(s))
}

type jsProviderTransportSettingsChangeListener struct{ cb js.Value }

func (self *jsProviderTransportSettingsChangeListener) ProviderTransportSettingsChanged(s *sdk.TransportSettings) {
	self.cb.Invoke(jsTransportSettings(s))
}

type jsTransportStatusChangeListener struct{ cb js.Value }

func (self *jsTransportStatusChangeListener) TransportStatusChanged(status *sdk.TransportStatus) {
	self.cb.Invoke(jsTransportStatus(status))
}

type jsProviderTransportStatusChangeListener struct{ cb js.Value }

func (self *jsProviderTransportStatusChangeListener) ProviderTransportStatusChanged(status *sdk.TransportStatus) {
	self.cb.Invoke(jsTransportStatus(status))
}

type jsDnsResolverSettingsChangeListener struct{ cb js.Value }

func (self *jsDnsResolverSettingsChangeListener) DnsResolverSettingsChanged(s *sdk.DnsResolverSettings) {
	self.cb.Invoke(jsDnsResolverSettings(s))
}

// ── performance profile ──────────────────────────────────────────────────────

// Mirrors sdk.PerformanceProfile. null for a nil profile, which the sdk reads
// as auto with every flag off; windowSize is null when the profile carries
// none (auto)
func jsPerformanceProfile(performanceProfile *sdk.PerformanceProfile) js.Value {
	if performanceProfile == nil {
		return js.Null()
	}
	var windowSize any
	if s := performanceProfile.WindowSize; s != nil {
		windowSize = map[string]any{
			"windowSizeMin":            s.WindowSizeMin,
			"windowSizeMinP2pOnly":     s.WindowSizeMinP2pOnly,
			"windowSizeMax":            s.WindowSizeMax,
			"windowSizeHardMax":        s.WindowSizeHardMax,
			"windowSizeReconnectScale": s.WindowSizeReconnectScale,
			"keepHealthiestCount":      s.KeepHealthiestCount,
			"ulimit":                   s.Ulimit,
		}
	}
	return js.ValueOf(map[string]any{
		"windowType":            performanceProfile.WindowType,
		"windowSize":            windowSize,
		"allowDirect":           performanceProfile.AllowDirect,
		"postQuantumEncryption": performanceProfile.PostQuantumEncryption,
	})
}

// Reads a profile from a JS object ({windowType, windowSize: {windowSizeMin,
// windowSizeMax, ...} | null, allowDirect, postQuantumEncryption}); null reads
// as nil, the sdk's auto profile. As in the sdk, a window with min == max is a
// fixed window of that many exits (the apps' Fixed IP is 1..1). A window the
// multi client would refuse (a negative size, or max below min) is rejected
// (ok false) instead of reaching the device
func parsePerformanceProfile(v js.Value) (*sdk.PerformanceProfile, bool) {
	if v.IsNull() || v.IsUndefined() {
		return nil, true
	}
	if v.Type() != js.TypeObject {
		return nil, false
	}
	performanceProfile := &sdk.PerformanceProfile{
		WindowType: sdk.WindowTypeAuto,
	}
	if x := v.Get("windowType"); x.Type() == js.TypeString {
		performanceProfile.WindowType = x.String()
	}
	if x := v.Get("allowDirect"); x.Type() == js.TypeBoolean {
		performanceProfile.AllowDirect = x.Bool()
	}
	if x := v.Get("postQuantumEncryption"); x.Type() == js.TypeBoolean {
		performanceProfile.PostQuantumEncryption = x.Bool()
	}
	if w := v.Get("windowSize"); w.Type() == js.TypeObject {
		intValue := func(key string) int {
			if x := w.Get(key); x.Type() == js.TypeNumber {
				return x.Int()
			}
			return 0
		}
		windowSize := &sdk.WindowSizeSettings{
			WindowSizeMin:        intValue("windowSizeMin"),
			WindowSizeMinP2pOnly: intValue("windowSizeMinP2pOnly"),
			WindowSizeMax:        intValue("windowSizeMax"),
			WindowSizeHardMax:    intValue("windowSizeHardMax"),
			KeepHealthiestCount:  intValue("keepHealthiestCount"),
			Ulimit:               intValue("ulimit"),
		}
		if x := w.Get("windowSizeReconnectScale"); x.Type() == js.TypeNumber {
			windowSize.WindowSizeReconnectScale = x.Float()
		}
		if windowSize.WindowSizeMin < 0 || windowSize.WindowSizeMax < windowSize.WindowSizeMin {
			return nil, false
		}
		performanceProfile.WindowSize = windowSize
	}
	return performanceProfile, true
}

// Hands each performance profile change to a page callback.
type jsPerformanceProfileChangeListener struct{ cb js.Value }

// Calls the page with the profile in force, as jsPerformanceProfile renders it.
func (self *jsPerformanceProfileChangeListener) PerformanceProfileChanged(performanceProfile *sdk.PerformanceProfile) {
	self.cb.Invoke(jsPerformanceProfile(performanceProfile))
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

// parseConnectLocationId accepts both id forms a page can hold: the bare
// location id the settings doc and the extension carry, and the composite
// ConnectLocationId json the sdk hands out (jsConnectLocation's
// connectLocationId), which is the only form that names a location GROUP or a
// device. Before this the composite form failed ParseId and the pick was
// silently dropped.
func parseConnectLocationId(s string) (*sdk.ConnectLocationId, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "{") {
		var id sdk.ConnectLocationId
		if err := json.Unmarshal([]byte(s), &id); err != nil {
			return nil, err
		}
		return &id, nil
	}
	id, err := sdk.ParseId(s)
	if err != nil {
		return nil, err
	}
	return &sdk.ConnectLocationId{LocationId: id}, nil
}

// NewPlatformDeviceRemote(apiUrl, platformUrl, byJwt, proxyUrl, signedProxyId, instanceId)
// builds a DeviceRemote that controls a hosted DeviceLocal by connecting
// directly to the proxy host at wss://<proxyUrl>/device-rpc, authenticating
// with the device's signed proxy id (not a jwt). byJwt is the network member
// jwt for the network space api. instanceId is the exact hosted DeviceLocal
// instance returned by /network/auth-client; inventing one makes strict RPC
// pairing reject every sync.
func NewPlatformDeviceRemote(this js.Value, args []js.Value) any {
	if len(args) < 6 {
		return js.ValueOf(map[string]any{
			"error": "hosted device instance_id is required",
		})
	}
	apiUrl := args[0].String()
	platformUrl := args[1].String()
	byJwt := args[2].String()
	proxyUrl := args[3].String()
	signedProxyId := args[4].String()
	instanceId, err := sdk.ParseId(args[5].String())
	if err != nil {
		return js.ValueOf(map[string]any{
			"error": "invalid hosted device instance_id: " + err.Error(),
		})
	}

	networkSpace := sdk.NewUrlsNetworkSpace(apiUrl, platformUrl)

	device, err := sdk.NewPlatformDeviceRemote(networkSpace, byJwt, proxyUrl, signedProxyId, instanceId)
	if err != nil {
		return js.ValueOf(map[string]any{"error": err.Error()})
	}
	return jsDeviceRemote(device)
}

// NewExtensionDeviceRemote(apiUrl, platformUrl, byJwt, instanceId, transport)
// builds the ordinary SDK DeviceRemote while delegating its opaque rpc frames
// to a JavaScript transport. Device endpoint credentials are intentionally
// absent from this binding.
func NewExtensionDeviceRemote(this js.Value, args []js.Value) any {
	if len(args) < 5 {
		return js.ValueOf(map[string]any{
			"error": "hosted device instance_id and extension transport are required",
		})
	}
	apiUrl := args[0].String()
	platformUrl := args[1].String()
	byJwt := args[2].String()
	instanceId, err := sdk.ParseId(args[3].String())
	if err != nil {
		return js.ValueOf(map[string]any{
			"error": "invalid hosted device instance_id: " + err.Error(),
		})
	}

	networkSpace := sdk.NewUrlsNetworkSpace(apiUrl, platformUrl)
	device, err := sdk.NewExtensionDeviceRemote(networkSpace, byJwt, instanceId, args[4])
	if err != nil {
		return js.ValueOf(map[string]any{"error": err.Error()})
	}
	return jsDeviceRemote(device)
}
