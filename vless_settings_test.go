package sdk

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// A synthetic reality public key: 32 bytes, base64url.
var testVlessPublicKey = connect.EncodeVlessPublicKey(bytes.Repeat([]byte{0x24}, 32))

const testVlessId = "5783a3e7-e373-51cd-8642-c83782b807c5"

func testVlessSettings() *VlessSettings {
	return &VlessSettings{
		Enabled:     true,
		Name:        "home",
		Address:     "203.0.113.20",
		Port:        443,
		Id:          testVlessId,
		Flow:        connect.VlessFlowVision,
		Network:     connect.VlessNetworkTcp,
		Security:    connect.VlessSecurityReality,
		ServerName:  "www.cover.example",
		Fingerprint: "chrome",
		PublicKey:   testVlessPublicKey,
		ShortId:     "0123abcd",
	}
}

// A pasted link reads into enabled settings, and the settings render back to a
// link that reads the same.
func TestVlessSettingsLinkRoundTrip(t *testing.T) {
	link := "vless://" + testVlessId + "@203.0.113.20:443?type=tcp&security=reality&flow=xtls-rprx-vision" +
		"&sni=www.cover.example&fp=chrome&pbk=" + testVlessPublicKey + "&sid=0123abcd#home"
	result := ParseVlessLink(link)
	if result.Error != "" {
		t.Fatalf("error = %s", result.Error)
	}
	if *result.Settings != *testVlessSettings() {
		t.Fatalf("settings = %+v, expected %+v", result.Settings, testVlessSettings())
	}
	again := ParseVlessLink(VlessSettingsLink(result.Settings))
	if again.Error != "" || *again.Settings != *result.Settings {
		t.Fatalf("round trip = %+v %s", again.Settings, again.Error)
	}

	ws := ParseVlessLink("vless://" + testVlessId + "@cdn.example:8443?type=ws&security=tls&path=%2Fws&host=front.example&alpn=h2,http/1.1&allowInsecure=1")
	if ws.Error != "" {
		t.Fatal(ws.Error)
	}
	if ws.Settings.Network != connect.VlessNetworkWs || ws.Settings.Path != "/ws" || ws.Settings.Host != "front.example" ||
		ws.Settings.Alpn != "h2,http/1.1" || !ws.Settings.AllowInsecure || ws.Settings.Security != connect.VlessSecurityTls {
		t.Fatalf("ws settings = %+v", ws.Settings)
	}
}

// Every problem maps to the localization key id the screens show, and a bad
// link is never read as something else.
func TestVlessSettingsErrorIds(t *testing.T) {
	cases := []struct {
		edit    func(settings *VlessSettings)
		errorId string
	}{
		{edit: func(settings *VlessSettings) { settings.Address = "" }, errorId: VlessErrorAddressInvalid},
		{edit: func(settings *VlessSettings) { settings.Address = "has space.example" }, errorId: VlessErrorAddressInvalid},
		{edit: func(settings *VlessSettings) { settings.Port = 0 }, errorId: VlessErrorPortInvalid},
		{edit: func(settings *VlessSettings) { settings.Port = 65536 }, errorId: VlessErrorPortInvalid},
		{edit: func(settings *VlessSettings) { settings.Id = "" }, errorId: VlessErrorIdInvalid},
		{edit: func(settings *VlessSettings) { settings.Network = "grpc" }, errorId: VlessErrorNetworkUnsupported},
		{edit: func(settings *VlessSettings) { settings.Security = "xtls" }, errorId: VlessErrorSecurityUnsupported},
		{edit: func(settings *VlessSettings) { settings.Network = connect.VlessNetworkWs }, errorId: VlessErrorFlowInvalid},
		{edit: func(settings *VlessSettings) { settings.Fingerprint = "netscape" }, errorId: VlessErrorFingerprintUnsupported},
		{edit: func(settings *VlessSettings) { settings.ServerName = "" }, errorId: VlessErrorServerNameRequired},
		{edit: func(settings *VlessSettings) { settings.PublicKey = "not a key" }, errorId: VlessErrorPublicKeyInvalid},
		{edit: func(settings *VlessSettings) { settings.ShortId = "xyz" }, errorId: VlessErrorShortIdInvalid},
		{edit: func(settings *VlessSettings) { settings.ShortId = "0123456789abcdef01" }, errorId: VlessErrorShortIdInvalid},
	}
	for i, c := range cases {
		settings := testVlessSettings()
		c.edit(settings)
		if errorId := ValidateVlessSettings(settings); errorId != c.errorId {
			t.Errorf("case %d: error = %q, expected %q", i, errorId, c.errorId)
		}
		if link := VlessSettingsLink(settings); link != "" {
			t.Errorf("case %d: invalid settings rendered a link %q", i, link)
		}
	}
	if errorId := ValidateVlessSettings(testVlessSettings()); errorId != "" {
		t.Fatalf("valid settings: %s", errorId)
	}
	// whitespace a form leaves and an address typed in brackets are fine
	untidy := testVlessSettings()
	untidy.Address = " [2001:db8::20] "
	untidy.Id = " " + testVlessId + " "
	untidy.Security = " REALITY "
	untidy.PublicKey = testVlessPublicKey + "\n"
	if errorId := ValidateVlessSettings(untidy); errorId != "" {
		t.Fatalf("untidy settings: %s", errorId)
	}
	for _, link := range []string{"", "https://vless.example", "vless://" + testVlessId + "@vless.example:443?type=grpc"} {
		result := ParseVlessLink(link)
		if result.Error == "" || result.Settings != nil {
			t.Errorf("%q: result = %+v", link, result)
		}
	}
	if result := ParseVlessLink("vless://" + testVlessId + "@vless.example:443?encryption=mlkem768x25519plus.native.0rtt.x"); result.Error != VlessErrorLinkUnsupported {
		t.Errorf("an encrypted link = %q, expected %q", result.Error, VlessErrorLinkUnsupported)
	}
}

// The settings ride the space's json, and a space without them writes none.
func TestVlessSettingsJson(t *testing.T) {
	values := NetworkSpaceValues{Vless: testVlessSettings()}
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"vless":{"enabled":true`) {
		t.Fatalf("json = %s", encoded)
	}
	var decoded NetworkSpaceValues
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Vless == nil || *decoded.Vless != *testVlessSettings() {
		t.Fatalf("decoded = %+v", decoded.Vless)
	}
	empty, err := json.Marshal(NetworkSpaceValues{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), "vless") {
		t.Fatalf("a space without VLESS wrote %s", empty)
	}
}

// Saving applies in place -- the space and everything bound to it survive --
// and replaces the strategy's VLESS dialer; turning it off removes the dialer
// and keeps the form; invalid enabled settings save nothing; the settings
// survive a restart of the manager.
func TestNetworkSpaceSetVlessSettings(t *testing.T) {
	storagePath, err := os.MkdirTemp("", "test_vless_settings")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(storagePath) })

	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	t.Cleanup(networkSpaceManager.Close)
	key := NewNetworkSpaceKey("space.example", "main")
	networkSpace := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {})

	if settings := networkSpace.GetVlessSettings(); *settings != *NewVlessSettings() {
		t.Fatalf("a space without VLESS = %+v, expected the new-form defaults", settings)
	}
	if n := len(networkSpace.clientStrategy.VlessConfigs()); n != 0 {
		t.Fatalf("dialers = %d before any settings", n)
	}

	if errorId := networkSpace.SetVlessSettings(testVlessSettings()); errorId != "" {
		t.Fatal(errorId)
	}
	if networkSpaceManager.GetNetworkSpace(key) != networkSpace {
		t.Fatal("saving VLESS settings replaced the space")
	}
	vlessConfigs := networkSpace.clientStrategy.VlessConfigs()
	if len(vlessConfigs) != 1 || vlessConfigs[0].Address != "203.0.113.20" || vlessConfigs[0].Flow != connect.VlessFlowVision {
		t.Fatalf("dialers = %+v", vlessConfigs)
	}
	if settings := networkSpace.GetVlessSettings(); *settings != *testVlessSettings() {
		t.Fatalf("settings = %+v", settings)
	}

	// the copy handed out is the caller's
	handedOut := networkSpace.GetVlessSettings()
	handedOut.Address = "198.51.100.1"
	if networkSpace.GetVlessSettings().Address != "203.0.113.20" {
		t.Fatal("editing a returned copy changed the space")
	}

	invalid := testVlessSettings()
	invalid.Port = 0
	if errorId := networkSpace.SetVlessSettings(invalid); errorId != VlessErrorPortInvalid {
		t.Fatalf("error = %q", errorId)
	}
	if networkSpace.GetVlessSettings().Port != 443 {
		t.Fatal("invalid settings were saved")
	}

	off := testVlessSettings()
	off.Enabled = false
	off.Port = 0
	if errorId := networkSpace.SetVlessSettings(off); errorId != "" {
		t.Fatalf("settings that are off are kept as they are: %s", errorId)
	}
	if n := len(networkSpace.clientStrategy.VlessConfigs()); n != 0 {
		t.Fatalf("dialers = %d after turning VLESS off", n)
	}
	if settings := networkSpace.GetVlessSettings(); settings.Enabled || settings.Address != "203.0.113.20" {
		t.Fatalf("settings after turning off = %+v", settings)
	}

	if errorId := networkSpace.SetVlessSettings(testVlessSettings()); errorId != "" {
		t.Fatal(errorId)
	}
	networkSpaceManager.Close()
	restored := NewNetworkSpaceManager(storagePath)
	t.Cleanup(restored.Close)
	restoredSpace := restored.GetNetworkSpace(key)
	if restoredSpace == nil {
		t.Fatal("the space did not survive the restart")
	}
	if settings := restoredSpace.GetVlessSettings(); *settings != *testVlessSettings() {
		t.Fatalf("restored settings = %+v", settings)
	}
	// a space built from stored settings starts with the dialer
	if n := len(restoredSpace.clientStrategy.VlessConfigs()); n != 1 {
		t.Fatalf("restored dialers = %d", n)
	}
	spaceJson, err := restoredSpace.ToJson()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spaceJson, `"vless":`) {
		t.Fatalf("the space json carries no VLESS settings: %s", spaceJson)
	}

	// nil clears them
	if errorId := restoredSpace.SetVlessSettings(nil); errorId != "" {
		t.Fatal(errorId)
	}
	if n := len(restoredSpace.clientStrategy.VlessConfigs()); n != 0 {
		t.Fatalf("dialers after clearing = %d", n)
	}
	if restoredSpace.valuesCopy().Vless != nil {
		t.Fatal("clearing left settings behind")
	}
}

// Saving an unchanged form stops before the manager, which would otherwise
// produce a new space generation.
func TestNetworkSpaceSetVlessSettingsUnchanged(t *testing.T) {
	networkSpaceManager := NewNetworkSpaceManagerNoStorage()
	t.Cleanup(networkSpaceManager.Close)
	key := NewNetworkSpaceKey("space.example", "main")
	networkSpace := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {
		values.Vless = testVlessSettings()
	})
	if networkSpace.updateInPlaceValues(func(values *NetworkSpaceValues) {
		values.Vless = testVlessSettings()
	}) {
		t.Fatal("an unchanged form reported a change")
	}
	if networkSpaceManager.GetNetworkSpace(key) != networkSpace {
		t.Fatal("an unchanged form replaced the space")
	}
}

// A VLESS change alone is an in-place change; together with a value that
// needs a rebuild it is not.
func TestOnlyInPlaceValuesChangedIncludesVless(t *testing.T) {
	key := NewNetworkSpaceKey("space.example", "main")
	previous := NetworkSpaceValues{ApiUrl: "https://api.space.example"}
	next := previous
	next.Vless = testVlessSettings()
	if !onlyInPlaceValuesChanged(key, &previous, &next) {
		t.Fatal("a VLESS change alone must apply in place")
	}
	rebuilt := next
	rebuilt.ApiUrl = "https://api2.space.example"
	if onlyInPlaceValuesChanged(key, &previous, &rebuilt) {
		t.Fatal("a VLESS change with an api change must rebuild")
	}
	if onlyInPlaceValuesChanged(key, &previous, &previous) {
		t.Fatal("no change is not an in-place change")
	}
}

// A hosted device's private strategy refuses the VLESS server in force, and
// one set on it later: VLESS is not cloud safe.
func TestHostedClientStrategyRefusesVless(t *testing.T) {
	networkSpaceManager := NewNetworkSpaceManagerNoStorage()
	t.Cleanup(networkSpaceManager.Close)
	key := NewNetworkSpaceKey("space.example", "main")
	networkSpace := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {})
	if errorId := networkSpace.SetVlessSettings(testVlessSettings()); errorId != "" {
		t.Fatal(errorId)
	}
	if n := len(networkSpace.clientStrategy.VlessConfigs()); n != 1 {
		t.Fatalf("space strategy dialers = %d", n)
	}
	strategy := networkSpace.newHostedClientStrategy(nil)
	defer strategy.Close()
	if n := len(strategy.VlessConfigs()); n != 0 {
		t.Fatalf("hosted strategy dialers = %d", n)
	}
	strategy.SetVlessConfigs(spaceVlessConfigs(testVlessSettings()))
	if n := len(strategy.VlessConfigs()); n != 0 {
		t.Fatalf("hosted strategy dialers after a set = %d", n)
	}
}
