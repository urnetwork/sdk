//go:build js

package main

import (
	"syscall/js"
	"testing"

	"github.com/urnetwork/sdk/v2026"
)

// The browser's connect view controller carries the location match in the
// shape src/types.ts declares: match, mixed, name, and locationId only when
// the sdk has one.
func TestLocationMatchWasmShape(t *testing.T) {
	locationId := sdk.NewId()
	value := jsLocationMatch(&sdk.LocationMatch{
		Match:      sdk.LocationMatchRegion,
		Mixed:      true,
		Name:       "Region A",
		LocationId: locationId,
	})
	if value.Get("match").String() != "region" ||
		!value.Get("mixed").Bool() ||
		value.Get("name").String() != "Region A" ||
		value.Get("locationId").String() != locationId.String() {
		t.Fatal("the location match differs from the declaration")
	}
	exact := jsLocationMatch(&sdk.LocationMatch{})
	if exact.Get("match").String() != "" ||
		exact.Get("mixed").Bool() ||
		exact.Get("name").String() != "" ||
		exact.Get("locationId").Type() != js.TypeUndefined {
		t.Fatal("an exact match carries a fallback")
	}
	if !jsLocationMatch(nil).IsNull() {
		t.Fatal("a missing match is not null")
	}
}

// getLocationMatch and addLocationMatchListener are bound on the connect view
// controller, and the listener is a signal to re-read getLocationMatch.
func TestConnectViewControllerWasmBindsTheLocationMatch(t *testing.T) {
	vc := jsConnectViewController(&sdk.ConnectViewController{}, func() {})
	for _, name := range []string{"getLocationMatch", "addLocationMatchListener"} {
		if vc.Get(name).Type() != js.TypeFunction {
			t.Fatalf("%s is not bound", name)
		}
	}
	if vc.Call("getLocationMatch").Get("match").String() != "" {
		t.Fatal("a view controller with no connection reports a fallback")
	}

	calls := 0
	argumentCount := -1
	cb := js.FuncOf(func(this js.Value, args []js.Value) any {
		calls += 1
		argumentCount = len(args)
		return nil
	})
	defer cb.Release()
	listener := &jsLocationMatchListener{cb: cb.Value}
	listener.LocationMatchChanged()
	if calls != 1 || argumentCount != 0 {
		t.Fatalf("the listener was called %d times with %d arguments, want one signal", calls, argumentCount)
	}
}
