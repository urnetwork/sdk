// The device settings cross the c abi as json, which is how a c host sets
// every device option the explicit constructors do not take: the shape the
// header documents carries no Go-only field, and urnet_new_device_local
// decodes its json over the defaults, so a partial or NULL json still builds a
// device (exports_device_local_settings_test.go builds them).
package main

import (
	"strings"
	"testing"
)

func TestDeviceLocalSettingsJsonShapeAndDecode(t *testing.T) {
	header := testingGeneratedFile(t, "..", "include", "urnetwork_sdk.h")
	start := strings.Index(header, "/* DeviceLocalSettings (json):\n")
	if start < 0 {
		t.Fatal("the header documents no DeviceLocalSettings json shape")
	}
	shape := header[start:]
	shape = shape[:strings.Index(shape, " */")]
	for _, key := range []string{
		"ClientCredentials",
		"ClientControl",
		"ProviderDiscovery",
		"LocalApi",
		"GeneratorFunc",
		"MultiClientIdentityStore",
		"ProviderDialContextSettings",
		"KeyMaterial",
	} {
		if strings.Contains(shape, " *   "+key+":") {
			t.Errorf("the DeviceLocalSettings json shape documents the Go-only field %s", key)
		}
	}
	for _, text := range []string{
		" *   SendTimeout: number (ns)\n",
		" *   ProvideExtenderEnabled: boolean\n",
		" *   DefaultProvideExtender: boolean\n",
		" * urnet_default_device_local_settings(): a field it omits keeps its default,\n",
	} {
		if !strings.Contains(shape, text) {
			t.Errorf("the DeviceLocalSettings json shape does not carry %q", text)
		}
	}

	exports := testingGeneratedFile(t, "..", "exports_gen.go")
	start = strings.Index(exports, "func urnet_new_device_local(")
	if start < 0 {
		t.Fatal("exports_gen.go has no urnet_new_device_local")
	}
	export := exports[start:]
	export = export[:strings.Index(export, "\n}\n")]
	if !strings.Contains(export, "settings_ := sdk.DefaultDeviceLocalSettings()") {
		t.Error("urnet_new_device_local does not decode its settings json over the defaults")
	}
}
