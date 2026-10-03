// Keep nullable current-location data separate from checked-operation failure
// through the actual generator, without changing the compatibility getter.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The additive C method preserves the old JSON result and introduces only the
// already-established error output convention.
func TestCheckedRemoteLocationBindingKeepsLegacyAndError(t *testing.T) {
	g := testingPreferenceGenerator(t)
	// The compatibility getter belongs to Device and is inherited by DeviceRemote.
	g.emitType(testingPreferenceType(t, g, "Device"))
	g.emitType(testingPreferenceType(t, g, "DeviceRemote"))
	legacy := testingPreferenceExport(t, g, "Device", "GetConnectLocation")
	checked := testingPreferenceExport(t, g, "DeviceRemote", "GetConnectLocationChecked")
	if legacy.cDecl != "char* urnet_device_get_connect_location(uint64_t self);" || legacy.sig.hasError || !g.deviceDerived["DeviceRemote"] {
		t.Fatal("checked location changed the legacy getter ABI")
	}
	if checked.cDecl != "char* urnet_device_remote_get_connect_location_checked(uint64_t self, char** out_error);" ||
		!checked.sig.hasError || checked.sig.result == nil || checked.sig.result.kind != kindJson ||
		!checked.sig.result.pointer || checked.sig.result.named == nil || checked.sig.result.named.Obj().Name() != "ConnectLocation" {
		t.Fatal("checked location lost its nullable data/error ABI")
	}
	errorGuard := "r0, err := self_.GetConnectLocationChecked()\n\tif err != nil {\n\t\tsetErrorOut(outError, err)\n\t\treturn nil\n\t}"
	nilGuard := "if r0 == nil {\n\t\treturn nil\n\t}"
	errorIndex := strings.Index(checked.goCode, errorGuard)
	nilIndex := strings.Index(checked.goCode, nilGuard)
	if errorIndex < 0 || nilIndex <= errorIndex {
		t.Fatal("checked location serialized absence before preserving the operation error")
	}
}

// Fresh C++ generation must throw a reported operation failure before mapping
// a successful null value to nullopt. This is not a native runtime assertion.
func TestCheckedRemoteLocationGeneratedCppDistinguishesFailureAndNil(t *testing.T) {
	g := testingPreferenceGenerator(t)
	output := t.TempDir()
	t.Chdir(output)
	if err := g.run(); err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(filepath.Join(output, "include", "urnetwork_sdk.hpp"))
	if err != nil {
		t.Fatal(err)
	}
	cpp := string(bytes)
	_, tail, found := strings.Cut(cpp, "inline std::optional<ConnectLocation> DeviceRemote::getConnectLocationChecked() const {\n")
	body, _, ended := strings.Cut(tail, "\n}\n")
	if !found || !ended || !strings.Contains(body, "char* r_c = urnet_device_remote_get_connect_location_checked(handle(), &err_c);") {
		t.Fatal("fresh C++ generation omitted the checked location call")
	}
	errorIndex := strings.Index(body, "if (err_c) {\n\t\tdetail::throwError(err_c);\n\t}")
	nilIndex := strings.Index(body, "if (!r_s) {\n\t\treturn std::nullopt;\n\t}")
	if errorIndex < 0 || nilIndex <= errorIndex || !strings.Contains(body, "return detail::parseJson<ConnectLocation>(r_s->c_str());") {
		t.Fatal("generated C++ conflated a failed observation with successful nil")
	}
	if !strings.Contains(cpp, "class DeviceRemote final : public Device {\n") ||
		!strings.Contains(cpp, "inline std::optional<ConnectLocation> Device::getConnectLocation() const {\n") {
		t.Fatal("fresh generation removed the inherited legacy current-location getter")
	}
	exports, err := os.ReadFile(filepath.Join(output, "include", "urnetwork_sdk.def"))
	if err != nil {
		t.Fatal(err)
	}
	exportLines := "\n" + string(exports) + "\n"
	if !strings.Contains(exportLines, "\n\turnet_device_get_connect_location\n") ||
		strings.Contains(exportLines, "\n\turnet_device_remote_get_connect_location\n") {
		t.Fatal("fresh generation changed the legacy getter export ownership")
	}
	if !strings.Contains(exportLines, "\n\turnet_device_remote_get_connect_location_checked\n") {
		t.Fatal("fresh generation omitted the additive checked location export")
	}
}
