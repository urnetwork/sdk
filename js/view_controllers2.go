//go:build js

package main

import (
	"syscall/js"

	"github.com/urnetwork/sdk/v2026"
)

// More view-controller JS bindings (see view_controllers.go for conventions).
// These are the live/stateful controllers the web /app renders directly:
//   - BlockActionViewController      ad/tracker blocking + live allow/block stats
//   - LocationsViewController        grouped/promoted location browse with state
//   - DevicesViewController          the network's clients/devices, live
// (Account-plane controllers whose screens are already correct on REST — wallet,
// network user, preferences, referral code — are intentionally not rebound;
// their VCs are request/response equivalents of the endpoints those screens use.)

func jsStringList(list *sdk.StringList) js.Value {
	out := []any{}
	if list != nil {
		for i := 0; i < list.Len(); i += 1 {
			out = append(out, list.Get(i))
		}
	}
	return js.ValueOf(out)
}

// ── BlockActionViewController ────────────────────────────────────────────────

func jsBlockStats(s *sdk.BlockStats) js.Value {
	if s == nil {
		return js.Null()
	}
	return js.ValueOf(map[string]any{
		"allowedCount": s.AllowedCount,
		"blockedCount": s.BlockedCount,
	})
}

// jsBlockAction is one aggregated routing decision (a cluster of ips/hosts and
// whether it was blocked). This is what the "block ads & trackers" feed shows.
func jsBlockAction(a *sdk.BlockAction) js.Value {
	if a == nil {
		return js.Null()
	}
	m := map[string]any{
		"time":  a.Time,
		"block": a.Block,
		"local": a.Local,
		"ips":   jsStringList(a.Ips),
		"hosts": jsStringList(a.Hosts),
	}
	if a.BlockActionId != nil {
		m["blockActionId"] = a.BlockActionId.String()
	}
	return js.ValueOf(m)
}

func jsBlockActionList(list *sdk.BlockActionList) js.Value {
	out := []any{}
	if list != nil {
		for i := 0; i < list.Len(); i += 1 {
			out = append(out, jsBlockAction(list.Get(i)))
		}
	}
	return js.ValueOf(out)
}

func jsBlockActionViewController(vc *sdk.BlockActionViewController) js.Value {
	if vc == nil {
		return js.Null()
	}

	m := map[string]any{}

	m["close"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.Close()
		return js.Null()
	})
	m["start"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.Start()
		return js.Null()
	})
	m["stop"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.Stop()
		return js.Null()
	})

	m["getBlockStats"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsBlockStats(vc.GetBlockStats())
	})
	m["getBlockActions"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsBlockActionList(vc.GetBlockActions())
	})
	m["getWindowDurationSeconds"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(vc.GetWindowDurationSeconds())
	})
	m["setWindowDurationSeconds"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		if 0 < len(args) {
			vc.SetWindowDurationSeconds(args[0].Int())
		}
		return js.Null()
	})

	m["addBlockActionsListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddBlockActionsListener(&jsBlockActionsListener{cb}))
	})
	m["addBlockActionStatsListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddBlockActionStatsListener(&jsBlockActionStatsListener{cb}))
	})

	return js.ValueOf(m)
}

// both block listeners are signal-only (the JS side re-reads getBlockActions /
// getBlockStats on notify — same convention as the contract rows listener)
type jsBlockActionsListener struct{ cb js.Value }

func (self *jsBlockActionsListener) BlockActionsChanged() {
	self.cb.Invoke()
}

type jsBlockActionStatsListener struct{ cb js.Value }

func (self *jsBlockActionStatsListener) BlockActionStatsChanged() {
	self.cb.Invoke()
}

// ── LocationsViewController ──────────────────────────────────────────────────

func jsConnectLocationList(list *sdk.ConnectLocationList) js.Value {
	out := []any{}
	if list != nil {
		for i := 0; i < list.Len(); i += 1 {
			out = append(out, jsConnectLocation(list.Get(i)))
		}
	}
	return js.ValueOf(out)
}

// jsFilteredLocations groups the results the way the app's browse screen renders
// them: best matches, promoted, then countries / regions / cities / devices.
func jsFilteredLocations(f *sdk.FilteredLocations) js.Value {
	if f == nil {
		return js.Null()
	}
	return js.ValueOf(map[string]any{
		"bestMatches": jsConnectLocationList(f.BestMatches),
		"promoted":    jsConnectLocationList(f.Promoted),
		"countries":   jsConnectLocationList(f.Countries),
		"regions":     jsConnectLocationList(f.Regions),
		"cities":      jsConnectLocationList(f.Cities),
		"devices":     jsConnectLocationList(f.Devices),
	})
}

func jsLocationsViewController(vc *sdk.LocationsViewController) js.Value {
	if vc == nil {
		return js.Null()
	}

	m := map[string]any{}

	m["close"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.Close()
		return js.Null()
	})
	m["start"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.Start()
		return js.Null()
	})
	m["stop"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.Stop()
		return js.Null()
	})

	m["getFilteredLocations"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsFilteredLocations(vc.GetFilteredLocations())
	})
	m["getFilteredLocationState"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(string(vc.GetFilteredLocationState()))
	})
	m["filterLocations"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		filter := ""
		if 0 < len(args) && args[0].Type() == js.TypeString {
			filter = args[0].String()
		}
		vc.FilterLocations(filter)
		return js.Null()
	})

	m["addFilteredLocationsListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddFilteredLocationsListener(&jsFilteredLocationsListener{cb}))
	})

	return js.ValueOf(m)
}

type jsFilteredLocationsListener struct{ cb js.Value }

func (self *jsFilteredLocationsListener) FilteredLocationsChanged(
	locations *sdk.FilteredLocations,
	state sdk.FilterLocationsState,
) {
	self.cb.Invoke(jsFilteredLocations(locations), js.ValueOf(string(state)))
}

// ── DevicesViewController ────────────────────────────────────────────────────

// jsNetworkClientInfo is one client/device on the network. Connections carry
// the live connection state; the count + whether any are open is what the
// devices list needs to show online/offline.
func jsNetworkClientInfo(c *sdk.NetworkClientInfo) js.Value {
	if c == nil {
		return js.Null()
	}
	connectionCount := 0
	connected := false
	if c.Connections != nil {
		connectionCount = c.Connections.Len()
		for i := 0; i < c.Connections.Len(); i += 1 {
			if c.Connections.Get(i).DisconnectTime == nil {
				connected = true
				break
			}
		}
	}
	m := map[string]any{
		"deviceName":        c.DeviceName,
		"deviceSpec":        c.DeviceSpec,
		"deviceDescription": c.DeviceDescription,
		"provideMode":       int(c.ProvideMode),
		"connectionCount":   connectionCount,
		"connected":         connected,
	}
	if c.ClientId != nil {
		m["clientId"] = c.ClientId.String()
	}
	if c.DeviceId != nil {
		m["deviceId"] = c.DeviceId.String()
	}
	if c.CreateTime != nil {
		m["createTimeUnixMillis"] = c.CreateTime.UnixMilli()
	}
	return js.ValueOf(m)
}

func jsNetworkClientInfoList(list *sdk.NetworkClientInfoList) js.Value {
	out := []any{}
	if list != nil {
		for i := 0; i < list.Len(); i += 1 {
			out = append(out, jsNetworkClientInfo(list.Get(i)))
		}
	}
	return js.ValueOf(out)
}

func jsDevicesViewController(vc *sdk.DevicesViewController) js.Value {
	if vc == nil {
		return js.Null()
	}

	m := map[string]any{}

	m["close"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.Close()
		return js.Null()
	})
	m["start"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.Start()
		return js.Null()
	})
	m["stop"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.Stop()
		return js.Null()
	})

	m["addNetworkClientsListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddNetworkClientsListener(&jsNetworkClientsListener{cb}))
	})

	return js.ValueOf(m)
}

type jsNetworkClientsListener struct{ cb js.Value }

func (self *jsNetworkClientsListener) NetworkClientsChanged(list *sdk.NetworkClientInfoList) {
	self.cb.Invoke(jsNetworkClientInfoList(list))
}
