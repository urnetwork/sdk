package main

import (
	"go/types"
	"testing"
)

// Keep the actual mobile package views to signed scalar primitives that the
// pinned gobind supports; a native uint64 counter must not escape here.
func TestAuthLogoutEventUsesMobileSupportedTypes(t *testing.T) {
	for _, view := range []struct {
		goos string
		tags string
	}{
		{goos: "android", tags: "sdk_mobile_bind"},
		{goos: "ios", tags: "sdk_mobile_bind"},
		{goos: "ios", tags: "sdk_mobile_bind,ios_extension"},
	} {
		loaded := testingLoadMobileView(t, view.goos, view.tags)
		object := loaded.Types.Scope().Lookup("AuthLogoutEventListener")
		if object == nil {
			t.Fatalf("%s %s: missing event interface", view.goos, view.tags)
		}
		iface := object.Type().Underlying().(*types.Interface)
		if iface.NumMethods() != 1 {
			t.Fatal("event interface must carry origin and cause in one call")
		}
		signature := iface.Method(0).Type().(*types.Signature)
		if signature.Params().Len() != 2 ||
			!types.Identical(signature.Params().At(0).Type(), types.Typ[types.Int64]) ||
			!types.Identical(signature.Params().At(1).Type(), types.Typ[types.String]) {
			t.Fatal("event parameters must remain basic int64 and string")
		}
		for index := 0; index < signature.Params().Len(); index++ {
			if !testingGobindBindsType(loaded.Types, signature.Params().At(index).Type()) {
				t.Fatal("event parameter is unsupported by gobind")
			}
		}
		api := loaded.Types.Scope().Lookup("Api").Type()
		methods := types.NewMethodSet(types.NewPointer(api))
		current := methods.Lookup(loaded.Types, "IsAuthLogoutCurrent")
		if current == nil {
			t.Fatal("mobile Api lost event currentness")
		}
		currentSignature := current.Obj().Type().(*types.Signature)
		if currentSignature.Params().Len() != 1 ||
			!types.Identical(currentSignature.Params().At(0).Type(), types.Typ[types.Int64]) {
			t.Fatal("currentness must accept the same signed opaque generation")
		}
	}
}
