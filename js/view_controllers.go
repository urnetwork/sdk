//go:build js

package main

import (
	"syscall/js"

	"github.com/urnetwork/sdk/v2026"
)

// JS bindings for the SDK view controllers — the same layer the iOS/Android
// screens are built on, so the web app renders from the same source of truth
// instead of reimplementing the aggregation and state machines in JS.
//
// Opened from a DeviceRemote (the viewControllerManager is embedded in every
// device), e.g.:
//
//	const cvc = device.openConnectViewController()
//	const unsub = cvc.addConnectionStatusListener(() => render(cvc.getConnectionStatus()))
//	cvc.connectBestAvailable()
//	...
//	cvc.close()   // always close: the manager holds the vc until it is closed
//
// Conventions (same as device_remote.go):
//   - getters return JS values (bool/string/number/object/array) or null
//   - setters take a single JS argument and return null
//   - listener adders take a JS callback and return an unsubscribe function
//   - lists are returned as plain JS arrays (gomobile's exportedList is an
//     index/len pair; JS wants an array)

// ── ConnectViewController ────────────────────────────────────────────────────

// jsProviderGridPoint converts one grid point. EndTime is exposed as
// millisUntil (relative), which is what a UI animating a point out actually
// needs, plus the absolute unix millis.
func jsProviderGridPoint(p *sdk.ProviderGridPoint) js.Value {
	if p == nil {
		return js.Null()
	}
	m := map[string]any{
		"x":      int(p.X),
		"y":      int(p.Y),
		"state":  string(p.State),
		"active": p.Active,
	}
	if p.ClientId != nil {
		m["clientId"] = p.ClientId.String()
	}
	if p.EndTime != nil {
		m["endTimeUnixMillis"] = p.EndTime.UnixMilli()
		m["endTimeMillisUntil"] = p.EndTime.MillisUntil()
	}
	return js.ValueOf(m)
}

// jsConnectGrid converts the whole grid: its dimensions, the provider window
// sizing (target vs current — what drives the "filling up" animation), and the
// provider points.
func jsConnectGrid(grid *sdk.ConnectGrid) js.Value {
	if grid == nil {
		return js.Null()
	}
	points := []any{}
	if list := grid.GetProviderGridPointList(); list != nil {
		for i := 0; i < list.Len(); i += 1 {
			points = append(points, jsProviderGridPoint(list.Get(i)))
		}
	}
	return js.ValueOf(map[string]any{
		"width":             int(grid.GetWidth()),
		"height":            int(grid.GetHeight()),
		"windowTargetSize":  int(grid.GetWindowTargetSize()),
		"windowCurrentSize": int(grid.GetWindowCurrentSize()),
		"points":            points,
	})
}

// jsConnectViewController binds the connect state machine the app screens use:
// connection status (DISCONNECTED / CONNECTING / DESTINATION_SET / CONNECTED),
// the selected location, the provider grid, and connect/disconnect.
func jsConnectViewController(vc *sdk.ConnectViewController) js.Value {
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

	// state
	m["getConnected"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(vc.GetConnected())
	})
	m["getConnectionStatus"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(string(vc.GetConnectionStatus()))
	})
	m["getSelectedLocation"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsConnectLocation(vc.GetSelectedLocation())
	})
	m["getGrid"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsConnectGrid(vc.GetGrid())
	})

	// actions
	m["connect"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		if 0 < len(args) {
			vc.Connect(parseConnectLocation(args[0]))
		}
		return js.Null()
	})
	m["connectBestAvailable"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.ConnectBestAvailable()
		return js.Null()
	})
	m["disconnect"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.Disconnect()
		return js.Null()
	})

	// listeners
	m["addConnectionStatusListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddConnectionStatusListener(&jsConnectionStatusListener{cb}))
	})
	m["addSelectedLocationListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddSelectedLocationListener(&jsSelectedLocationListener{cb}))
	})
	m["addGridListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddGridListener(&jsGridListener{cb}))
	})

	return js.ValueOf(m)
}

type jsConnectionStatusListener struct{ cb js.Value }

func (self *jsConnectionStatusListener) ConnectionStatusChanged() {
	self.cb.Invoke()
}

type jsSelectedLocationListener struct{ cb js.Value }

func (self *jsSelectedLocationListener) SelectedLocationChanged(location *sdk.ConnectLocation) {
	self.cb.Invoke(jsConnectLocation(location))
}

type jsGridListener struct{ cb js.Value }

func (self *jsGridListener) GridChanged() {
	self.cb.Invoke()
}

// ── ContractDetailsViewController ────────────────────────────────────────────

// jsContractEntry converts one un-aggregated contract: its own used/total byte
// counts and bit rate. Contracts are never paired across directions.
func jsContractEntry(entry *sdk.ContractEntry) js.Value {
	if entry == nil {
		return js.Null()
	}
	return js.ValueOf(map[string]any{
		"contractId":     entry.ContractId,
		"usedByteCount":  entry.UsedByteCount,
		"totalByteCount": entry.TotalByteCount,
		"bitRate":        entry.BitRate,
		"hasStream":      entry.HasStream,
	})
}

func jsContractEntryList(list *sdk.ContractEntryList) js.Value {
	entries := []any{}
	if list != nil {
		for i := 0; i < list.Len(); i += 1 {
			entries = append(entries, jsContractEntry(list.Get(i)))
		}
	}
	return js.ValueOf(entries)
}

// jsContractPeerRow converts one peer's row: two independent newest-first
// stacks of contracts (send and receive), the per-direction cumulative bytes
// moved in the current run (reset when the peer goes idle), and the Closing
// eject flag.
func jsContractPeerRow(row *sdk.ContractPeerRow) js.Value {
	if row == nil {
		return js.Null()
	}
	return js.ValueOf(map[string]any{
		"clientId": row.ClientId,

		"sendContracts":    jsContractEntryList(row.SendContracts),
		"receiveContracts": jsContractEntryList(row.ReceiveContracts),

		"sendByteCount":    row.SendByteCount,
		"receiveByteCount": row.ReceiveByteCount,

		"lastActivityMillis": row.LastActivityMillis,

		"closing": row.Closing,
	})
}

func jsContractPeerRowList(list *sdk.ContractPeerRowList) js.Value {
	rows := []any{}
	if list != nil {
		for i := 0; i < list.Len(); i += 1 {
			rows = append(rows, jsContractPeerRow(list.Get(i)))
		}
	}
	return js.ValueOf(rows)
}

func jsContractClientRow(row *sdk.ContractClientRow) js.Value {
	if row == nil {
		return js.Null()
	}
	return js.ValueOf(map[string]any{
		"clientId": row.ClientId,

		"contractId":          row.ContractId,
		"companionContractId": row.CompanionContractId,

		"contractUsedByteCount": row.ContractUsedByteCount,
		"contractByteCount":     row.ContractByteCount,
		"contractBitRate":       row.ContractBitRate,

		"companionContractUsedByteCount": row.CompanionContractUsedByteCount,
		"companionContractByteCount":     row.CompanionContractByteCount,
		"companionContractBitRate":       row.CompanionContractBitRate,

		"pairCount": row.PairCount,
		"closing":   row.Closing,
	})
}

func jsContractClientRowList(list *sdk.ContractClientRowList) js.Value {
	rows := []any{}
	if list != nil {
		for i := 0; i < list.Len(); i += 1 {
			rows = append(rows, jsContractClientRow(list.Get(i)))
		}
	}
	return js.ValueOf(rows)
}

// jsContractDetailsViewController binds the shared per-contract rows source:
// per-peer grouping into send/receive stacks (newest first, stable order) and
// the closing lifecycle. The web app renders these rows exactly like the native
// apps do — none of that logic is reimplemented in JS.
func jsContractDetailsViewController(vc *sdk.ContractDetailsViewController) js.Value {
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

	m["getContractRows"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsContractPeerRowList(vc.GetContractRows())
	})
	// Deprecated aggregate projections retained with the old combined open
	// method for a compatibility release.
	m["getClientContractRows"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsContractClientRowList(vc.GetClientContractRows())
	})
	m["getProviderContractRows"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsContractClientRowList(vc.GetProviderContractRows())
	})

	// the view controller owns the at-top activity ordering; the app reports its
	// scroll position and reads the pending ("N new") count
	m["setAtTop"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		if 0 < len(args) {
			vc.SetAtTop(args[0].Truthy())
		}
		return js.Null()
	})
	m["pendingCount"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return vc.PendingCount()
	})

	m["addContractRowsListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddContractRowsListener(&jsContractRowsListener{cb}))
	})

	return js.ValueOf(m)
}

type jsContractRowsListener struct{ cb js.Value }

func (self *jsContractRowsListener) ContractRowsChanged() {
	self.cb.Invoke()
}

// ── ContractViewController ───────────────────────────────────────────────────
// Throughput over the window — the client feed and the PROVIDER feed (what the
// account's provider-statistics surface shows). Egress/ingress byte + bit-rate
// samples per time point, split remote/local/block.

func jsThroughputSample(s *sdk.ThroughputSample) js.Value {
	if s == nil {
		return js.Null()
	}
	return js.ValueOf(map[string]any{
		"egressByteCount":    s.EgressByteCount,
		"ingressByteCount":   s.IngressByteCount,
		"egressPacketCount":  s.EgressPacketCount,
		"ingressPacketCount": s.IngressPacketCount,
		"egressBitRate":      s.EgressBitRate,
		"ingressBitRate":     s.IngressBitRate,
	})
}

func jsThroughputPoint(p *sdk.ThroughputPoint) js.Value {
	if p == nil {
		return js.Null()
	}
	return js.ValueOf(map[string]any{
		"time":   p.Time,
		"remote": jsThroughputSample(p.Remote),
		"local":  jsThroughputSample(p.Local),
		"block":  jsThroughputSample(p.Block),
	})
}

func jsThroughputPointList(list *sdk.ThroughputPointList) js.Value {
	out := []any{}
	if list != nil {
		for i := 0; i < list.Len(); i += 1 {
			out = append(out, jsThroughputPoint(list.Get(i)))
		}
	}
	return js.ValueOf(out)
}

func jsPacketStats(s *sdk.PacketStats) js.Value {
	if s == nil {
		return js.Null()
	}
	return js.ValueOf(map[string]any{
		"remoteEgressByteCount":  s.RemoteEgressByteCount,
		"remoteIngressByteCount": s.RemoteIngressByteCount,
		"localEgressByteCount":   s.LocalEgressByteCount,
		"localIngressByteCount":  s.LocalIngressByteCount,
		"blockEgressByteCount":   s.BlockEgressByteCount,
		"blockIngressByteCount":  s.BlockIngressByteCount,
	})
}

func jsContractViewController(vc *sdk.ContractViewController) js.Value {
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

	m["getThroughputPoints"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsThroughputPointList(vc.GetThroughputPoints())
	})
	m["getProviderThroughputPoints"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsThroughputPointList(vc.GetProviderThroughputPoints())
	})
	m["getPacketStats"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsPacketStats(vc.GetPacketStats())
	})
	m["getProviderPacketStats"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsPacketStats(vc.GetProviderPacketStats())
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

	m["addThroughputListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddThroughputListener(&jsThroughputListener{cb}))
	})

	return js.ValueOf(m)
}

type jsThroughputListener struct{ cb js.Value }

func (self *jsThroughputListener) ThroughputChanged() {
	self.cb.Invoke()
}
