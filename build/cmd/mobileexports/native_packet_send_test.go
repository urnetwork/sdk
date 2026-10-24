// Go-only pooled batch ownership must not become a gomobile method in any
// binding view; native Android and Apple callers must retain the real method.
package main

import (
	"go/types"
	"testing"
)

// Source-loads the actual SDK and dependencies for every native/bind tag pair.
// The executor supplies the same pinned overlay through GOFLAGS so nested
// packages.Load observes the method's real platform build constraints.
func TestPrivateNativePacketBatchGoOnlyPlatformViews(t *testing.T) {
	for _, platform := range []struct{ goos, extension string }{
		{goos: "android"},
		{goos: "ios"},
		{goos: "ios", extension: "ios_extension"},
	} {
		for _, binding := range []bool{false, true} {
			tags := platform.extension
			if binding {
				tags = "sdk_mobile_bind"
				if platform.extension != "" {
					tags += "," + platform.extension
				}
			}
			pkg := testingLoadMobileView(t, platform.goos, tags)
			object := pkg.Types.Scope().Lookup("DeviceLocal")
			if object == nil {
				t.Fatalf("actual DeviceLocal is missing: %s %s", platform.goos, tags)
			}
			methods := types.NewMethodSet(types.NewPointer(object.Type()))
			native := methods.Lookup(nil, "SendNativePacketsNoCopy")
			if binding {
				if native != nil {
					t.Errorf("NATIVE_POOLED_BATCH_EXPOSED_TO_MOBILE: %s %s", platform.goos, tags)
				}
			} else if native == nil {
				t.Errorf("native Go batch entry is absent: %s %s", platform.goos, tags)
			} else {
				signature := native.Obj().Type().(*types.Signature)
				if signature.Params().Len() != 1 || signature.Results().Len() != 1 || signature.Variadic() ||
					!types.Identical(signature.Params().At(0).Type(), types.NewSlice(types.NewSlice(types.Typ[types.Uint8]))) ||
					!types.Identical(signature.Results().At(0).Type(), types.Typ[types.Int]) {
					t.Errorf("native Go batch ownership signature changed: %s %s", platform.goos, tags)
				}
			}
			if methods.Lookup(nil, "SendPacketBatch") == nil {
				t.Errorf("portable packed batch entry disappeared: %s %s", platform.goos, tags)
			}
		}
	}
}
