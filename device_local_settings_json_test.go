package sdk

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// The json form of the device settings, which the c abi carries
// (urnet_default_device_local_settings and urnet_new_device_local): the fields
// json can carry round trip, the Go-only ones stay out, a duration is its
// count of nanoseconds, and a json decoded over the defaults changes only the
// fields it names.

// The defaults encode, and decode back to the same json.
func TestDeviceLocalSettingsJsonRoundTrip(t *testing.T) {
	settingsBytes, err := json.Marshal(DefaultDeviceLocalSettings())
	if err != nil {
		t.Fatalf("the default settings do not encode: %v", err)
	}
	values := map[string]any{}
	if err := json.Unmarshal(settingsBytes, &values); err != nil {
		t.Fatal(err)
	}
	// the Go-only authorities and seams, the key material and the embedded
	// client settings (any of its fields) stay out
	for _, key := range []string{
		"ClientCredentials",
		"ClientControl",
		"ProviderDiscovery",
		"LocalApi",
		"GeneratorFunc",
		"MultiClientIdentityStore",
		"ProviderDialContextSettings",
		"KeyMaterial",
		"ClientSettings",
		"SendBufferSettings",
		"ReceiveBufferSettings",
	} {
		if _, ok := values[key]; ok {
			t.Errorf("the settings json carries %s", key)
		}
	}
	for _, key := range []string{"ProvideExtenderEnabled", "DefaultProvideExtender", "AllowProvider"} {
		if values[key] != true {
			t.Errorf("the settings json has %s = %v, expected true", key, values[key])
		}
	}
	// every app binds the extender's dns carrier on 4053 alone (L2)
	if value, ok := values["ProvideExtenderDnsPrivilegedPort"]; !ok || value != false {
		t.Errorf("the settings json has ProvideExtenderDnsPrivilegedPort = %v (present %t), expected false", value, ok)
	}
	// a duration is its count of nanoseconds
	if sendTimeout, ok := values["SendTimeout"].(float64); !ok || time.Duration(sendTimeout) != 5*time.Second {
		t.Errorf("the settings json has SendTimeout = %v, expected the nanoseconds of 5s", values["SendTimeout"])
	}

	decoded := &DeviceLocalSettings{}
	if err := json.Unmarshal(settingsBytes, decoded); err != nil {
		t.Fatal(err)
	}
	reencodedBytes, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reencodedBytes, settingsBytes) {
		t.Fatalf("the settings json did not round trip:\n%s\n%s", settingsBytes, reencodedBytes)
	}
}

// A json decoded over the defaults, as the c abi decodes one, changes only the
// fields it names. A key of the embedded client settings, which never cross,
// cannot clear them.
func TestDeviceLocalSettingsJsonDecodesOverTheDefaults(t *testing.T) {
	settings := DefaultDeviceLocalSettings()
	settingsJson := `{"DefaultProvideExtender": false, "SendTimeout": 2500000000, "SendBufferSettings": null}`
	if err := json.Unmarshal([]byte(settingsJson), settings); err != nil {
		t.Fatal(err)
	}
	if settings.DefaultProvideExtender {
		t.Error("the json did not turn the device default off")
	}
	if settings.SendTimeout != 2500*time.Millisecond {
		t.Errorf("SendTimeout = %s, expected the 2.5s the json named", settings.SendTimeout)
	}
	if !settings.ProvideExtenderEnabled || !settings.AllowProvider {
		t.Error("a field the json omits lost its default")
	}
	if settings.ClientSettings.SendBufferSettings == nil {
		t.Error("the json cleared the client settings")
	}
}

// The extender's 53 opt-in crosses as json like the other device controls: a
// c host that names it on decodes it over the defaults, which the sn miner's
// equivalent does in Go (L2), and the rest keep their defaults.
func TestDeviceLocalSettingsJsonCarriesTheExtenderDnsPrivilegedPort(t *testing.T) {
	settings := DefaultDeviceLocalSettings()
	if err := json.Unmarshal([]byte(`{"ProvideExtenderDnsPrivilegedPort": true}`), settings); err != nil {
		t.Fatal(err)
	}
	if !settings.ProvideExtenderDnsPrivilegedPort {
		t.Fatal("the json did not opt the device in to 53")
	}
	if !settings.ProvideExtenderEnabled || !settings.DefaultProvideExtender {
		t.Error("a field the json omits lost its default")
	}
	settingsBytes, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{}
	if err := json.Unmarshal(settingsBytes, &values); err != nil {
		t.Fatal(err)
	}
	if values["ProvideExtenderDnsPrivilegedPort"] != true {
		t.Fatalf("the settings json has ProvideExtenderDnsPrivilegedPort = %v, expected true",
			values["ProvideExtenderDnsPrivilegedPort"])
	}
}
