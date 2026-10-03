// New optional-read envelopes are additive. Existing checked calls retain
// their nullable ABI, while error-free result getters retain successful nil.
package main

import "testing"

// Read errors belong to the outer operation; absence belongs to its getter.
func TestOptionalStorageBindingSeparatesResultFromNullableValue(t *testing.T) {
	g := testingPreferenceGenerator(t)
	for _, name := range []string{"LocalState", "LocalAuthStateSnapshot", "LocalStateLocationReadResult", "LocalStateKeyMaterialReadResult"} {
		g.emitType(testingPreferenceType(t, g, name))
	}
	for _, expected := range []struct {
		receiver string
		method   string
		decl     string
	}{
		{receiver: "LocalState", method: "LoadConnectLocation", decl: "char* urnet_local_state_load_connect_location(uint64_t self, char** out_error);"},
		{receiver: "LocalState", method: "LoadDefaultLocation", decl: "char* urnet_local_state_load_default_location(uint64_t self, char** out_error);"},
		{receiver: "LocalAuthStateSnapshot", method: "LoadConnectLocation", decl: "char* urnet_local_auth_state_snapshot_load_connect_location(uint64_t self, char** out_error);"},
		{receiver: "LocalAuthStateSnapshot", method: "LoadDefaultLocation", decl: "char* urnet_local_auth_state_snapshot_load_default_location(uint64_t self, char** out_error);"},
		{receiver: "LocalState", method: "LoadDeviceLocalKeyMaterial", decl: "uint64_t urnet_local_state_load_device_local_key_material(uint64_t self, char** out_error);"},
	} {
		item := testingPreferenceExport(t, g, expected.receiver, expected.method)
		if item.cDecl != expected.decl || !item.sig.hasError {
			t.Fatalf("native-safe additive read changed the existing ABI: %s.%s", expected.receiver, expected.method)
		}
	}
	location := testingPreferenceExport(t, g, "LocalStateLocationReadResult", "GetLocation")
	if location.sig.hasError || location.sig.result == nil || !location.sig.result.pointer ||
		location.sig.result.kind != kindJson || location.sig.result.named.Obj().Name() != "ConnectLocation" ||
		location.cDecl != "char* urnet_local_state_location_read_result_get_location(uint64_t self);" {
		t.Fatal("location result getter lost its error-free optional value")
	}
	keys := testingPreferenceExport(t, g, "LocalStateKeyMaterialReadResult", "GetKeyMaterial")
	if keys.sig.hasError || keys.sig.result == nil || keys.sig.result.kind != kindHandle ||
		keys.sig.result.named.Obj().Name() != "DeviceLocalKeyMaterial" ||
		keys.cDecl != "uint64_t urnet_local_state_key_material_read_result_get_key_material(uint64_t self);" {
		t.Fatal("key result getter lost its error-free optional material")
	}
}
