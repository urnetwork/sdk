//go:build !ios && !android && !js && !sdk_mobile_bind

package sdk

// The census fragment for device_local_extender_native.go, which only the native desktop and
// server builds compile. A census entry names its value, so a value declared under a build
// constraint can only be named by a test file under the same constraint. The twin file carries
// the empty fragment for the builds that leave the native extender out (ios, android, js);
// within the test build the two constraints are complements, so exactly one is compiled.

// Values that device_local_extender_native.go declares.
var streamAdapterExtenderNativeValueCensus = map[string]streamAdapterPackageVar{
	"extenderProvideListenTimeout": streamAdapterPackageConstOf(extenderProvideListenTimeout),
}
