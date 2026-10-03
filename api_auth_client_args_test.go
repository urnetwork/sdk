package sdk

import (
	"encoding/json"
	"testing"
)

// The onboarding campaign reads the device's zone and locale off every
// auth-client call; the fields must reach the wire under the names the
// server and the spec use, and stay off it when unset.
func TestAuthNetworkClientArgsMarshalTimeZoneAndLocale(t *testing.T) {
	args := &AuthNetworkClientArgs{
		DeviceDescription: "test device",
		DeviceSpec:        "test spec",
		TimeZone:          "America/Chicago",
		Locale:            "pt-BR",
	}
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["time_zone"] != "America/Chicago" {
		t.Errorf("time_zone on the wire = %v, want America/Chicago", wire["time_zone"])
	}
	if wire["locale"] != "pt-BR" {
		t.Errorf("locale on the wire = %v, want pt-BR", wire["locale"])
	}

	var back AuthNetworkClientArgs
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.TimeZone != args.TimeZone || back.Locale != args.Locale {
		t.Errorf("round trip = %q/%q, want %q/%q", back.TimeZone, back.Locale, args.TimeZone, args.Locale)
	}

	// unset fields are omitted, so older servers see the same body as before
	b, err = json.Marshal(&AuthNetworkClientArgs{DeviceDescription: "d", DeviceSpec: "s"})
	if err != nil {
		t.Fatal(err)
	}
	wire = map[string]any{}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"time_zone", "locale"} {
		if _, present := wire[key]; present {
			t.Errorf("%s must be omitted when unset", key)
		}
	}
}
