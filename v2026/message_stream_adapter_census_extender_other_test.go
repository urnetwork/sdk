//go:build (ios || android || js) && !sdk_mobile_bind

package sdk

// The empty census fragment for the builds that do not compile device_local_extender_native.go.
// Before this file, the native extender's values were named in the portable census, so the
// package's tests did not compile for js (wasm). See the twin file for the native builds.

// Empty: these builds declare none of the native extender's values.
var streamAdapterExtenderNativeValueCensus = map[string]streamAdapterPackageVar{}
