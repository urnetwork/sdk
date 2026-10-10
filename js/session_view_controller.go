//go:build js

package main

import (
	"github.com/urnetwork/sdk/v2026"
	"syscall/js"
)

func jsClientSessionViewController(vc *sdk.ClientSessionViewController, closeController func()) js.Value {
	if vc == nil {
		return js.Null()
	}
	m := map[string]any{}
	m["close"] = jsViewControllerClose(closeController)
	m["start"] = js.FuncOf(func(js.Value, []js.Value) any { vc.Start(); return js.Null() })
	m["stop"] = js.FuncOf(func(js.Value, []js.Value) any { vc.Stop(); return js.Null() })
	m["refresh"] = js.FuncOf(func(js.Value, []js.Value) any { vc.Refresh(); return js.Null() })
	m["setVisible"] = js.FuncOf(func(_ js.Value, args []js.Value) any { vc.SetVisible(boolArg(args, 0)); return js.Null() })
	m["setForeground"] = js.FuncOf(func(_ js.Value, args []js.Value) any { vc.SetForeground(boolArg(args, 0)); return js.Null() })
	m["getSnapshot"] = js.FuncOf(func(js.Value, []js.Value) any { return jsJson(vc.GetSnapshot()) })
	m["revokeSession"] = js.FuncOf(func(_ js.Value, args []js.Value) any {
		if id, err := sdk.ParseId(stringArg(args, 0)); err == nil {
			vc.RevokeSession(id)
		}
		return js.Null()
	})
	m["revokeOtherSessions"] = js.FuncOf(func(js.Value, []js.Value) any { vc.RevokeOtherSessions(); return js.Null() })
	m["addClientSessionListener"] = js.FuncOf(func(_ js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddClientSessionListener(&jsClientSessionListener{cb}))
	})
	return js.ValueOf(m)
}

type jsClientSessionListener struct{ callback js.Value }

func (self *jsClientSessionListener) ClientSessionsChanged(snapshot *sdk.ClientSessionSnapshot) {
	self.callback.Invoke(jsJson(snapshot))
}
