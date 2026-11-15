//go:build js

package main

import (
	"encoding/json"
	"syscall/js"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

// sdk.Sidecar for the extension's hosts (sidecar §18.5's js row, §18.4's alarms-only extension, §18.8; D-SDK-30;
// SDK-M8-06). URnetworkNewSidecarRemote opens the remote backend over a host-supplied port to `ur native-host`, which
// relays status and events alone, so sdk.wasm links no sidecar root and stays under its gate
// (plan-SDK-small-readings #7); URnetworkNewSidecarLocal answers unsupported until the extension's own Worker (Later).
//
// The extension is alarms-only (§18.4): its approvals list nothing for it to decide, Decide answers
// APPROVAL_NOT_ELIGIBLE, and PreviewRevokeDevice and RevokeDevice answer REVOKE_NOT_CAPABLE here, before anything
// reaches the port, as `ur native-host` answers any call §5.8 signs only on a person's confirmation. Nothing here signs
// or relays a signing call.
//
// Conventions, as device_remote.go's: getters return JS values or null; a call that answers through a callback calls
// it once with a plain object through the result's json tags; listener adders return an unsubscribe function; a
// constructor that cannot open answers {error}.

// URnetworkNewSidecarRemote(port, presenceSigner, settings): a Sidecar over port, a chrome.runtime.Port from
// connectNative or anything shaped like one. presenceSigner is ignored: the extension signs nothing. settings is a
// SidecarSettings object, decoded over DefaultSidecarSettings.
func NewSidecarRemote(this js.Value, args []js.Value) any {
	if len(args) < 1 || args[0].Type() != js.TypeObject {
		return js.ValueOf(map[string]any{"error": "a port to ur native-host is required"})
	}
	settings := sdk.DefaultSidecarSettings()
	if 2 < len(args) && args[2].Type() == js.TypeObject {
		encoded := js.Global().Get("JSON").Call("stringify", args[2]).String()
		if err := json.Unmarshal([]byte(encoded), settings); err != nil {
			return js.ValueOf(map[string]any{"error": "the sidecar settings do not read: " + err.Error()})
		}
	}
	sidecar, err := sdk.NewSidecarRemote(args[0], nil, settings)
	if err != nil {
		return js.ValueOf(map[string]any{"error": err.Error()})
	}
	return jsSidecar(sidecar)
}

// URnetworkNewSidecarLocal: unsupported on js.
func NewSidecarLocal(this js.Value, args []js.Value) any {
	_, err := sdk.NewSidecarLocal(nil, nil, nil, nil)
	message := "a local sidecar is not supported on js"
	if err != nil {
		message = err.Error()
	}
	return js.ValueOf(map[string]any{"error": message})
}

// The Sidecar as a JS object.
func jsSidecar(sidecar *sdk.Sidecar) js.Value {
	m := map[string]any{}
	m["getState"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(sidecar.GetState())
	})
	m["getStateReason"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(sidecar.GetStateReason())
	})
	m["getSidecarId"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		if sidecarId := sidecar.GetSidecarId(); sidecarId != nil {
			return js.ValueOf(sidecarId.String())
		}
		return js.Null()
	})
	m["addEventListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		callback, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(sidecar.AddEventListener(&jsSidecarEventListener{callback: callback}))
	})
	m["unlock"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		sidecar.Unlock(stringArg(args, 0), jsSidecarResultCallback(args, 1))
		return js.Null()
	})
	m["setPushToken"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		sidecar.SetPushToken(stringArg(args, 0), stringArg(args, 1), jsSidecarResultCallback(args, 2))
		return js.Null()
	})
	m["getApprovals"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsSidecarApprovals()
	})
	m["close"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		sidecar.Close()
		return js.Null()
	})
	return js.ValueOf(m)
}

// Hands each event to a JS callback as a plain object.
type jsSidecarEventListener struct {
	callback js.Value
}

func (self *jsSidecarEventListener) SidecarEventReceived(event *sdk.SidecarEvent) {
	self.callback.Invoke(jsJson(event))
}

// The callback at args[i] as a SidecarResultCallback, called with the result's plain object; none calls nothing.
func jsSidecarResultCallback(args []js.Value, i int) sdk.SidecarResultCallback {
	var callback js.Value
	if i < len(args) && args[i].Type() == js.TypeFunction {
		callback = args[i]
	}
	return connect.NewApiCallback[*sdk.SidecarResult](func(result *sdk.SidecarResult, err error) {
		if callback.IsUndefined() || callback.IsNull() {
			return
		}
		if err != nil {
			result = &sdk.SidecarResult{Message: err.Error()}
		}
		callback.Invoke(jsJson(result))
	})
}

// The alarms-only approvals (the file's comment): nothing to decide, no devices to revoke from here.
func jsSidecarApprovals() js.Value {
	m := map[string]any{}
	m["getPending"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.Global().Get("Array").New()
	})
	m["addPendingListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		// no item is ever listed here, so nothing is ever heard
		return js.FuncOf(func(this js.Value, args []js.Value) any {
			return js.Null()
		}).Value
	})
	m["decide"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		jsSidecarResultCallback(args, 2).Result(&sdk.SidecarResult{
			Code:    sdk.SidecarCodeApprovalNotEligible,
			Message: "the extension decides nothing: approve on an app or with ur approve",
		}, nil)
		return js.Null()
	})
	m["getDevices"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		// unknown, never none
		return js.Null()
	})
	m["previewRevokeDevice"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		if 4 < len(args) && args[4].Type() == js.TypeFunction {
			args[4].Invoke(jsJson(&sdk.SidecarRevokePlan{
				Code:    sdk.SidecarCodeRevokeNotCapable,
				Message: "the extension revokes no device: revoke it on an app or with ur devices revoke",
			}))
		}
		return js.Null()
	})
	m["revokeDevice"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		jsSidecarResultCallback(args, 2).Result(&sdk.SidecarResult{
			Code:    sdk.SidecarCodeRevokeNotCapable,
			Message: "the extension revokes no device: revoke it on an app or with ur devices revoke",
		}, nil)
		return js.Null()
	})
	return js.ValueOf(m)
}
