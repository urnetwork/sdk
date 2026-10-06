// The provider extender's two device controls cross the c abi (EXTENDER.md
// G1, F3): the host-facing constructor that takes both, with its header
// prototype, module definition entry and c++ wrapper, the settings field in
// the json shape and the c++ struct, and the five language bindings that bind
// the header. Third-party provider apps build their device through these.
package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestProvideExtenderDeviceControlsCrossTheAbi(t *testing.T) {
	const cName = "urnet_new_device_local_with_provide_extender"

	exportedCNames, _ := testingCoverageReport(t)
	if exported := exportedCNames["NewDeviceLocalWithProvideExtender"]; exported != cName {
		t.Errorf("NewDeviceLocalWithProvideExtender is exported as %q, expected %s", exported, cName)
	}
	if !testingDefSymbols(t)[cName] {
		t.Errorf("the module definition does not name %s", cName)
	}

	header := testingGeneratedFile(t, "..", "include", "urnetwork_sdk.h")
	for _, text := range []string{
		"uint64_t urnet_new_device_local_with_provide_extender(uint64_t network_space, const char* by_jwt, const char* device_description, const char* device_spec, const char* app_version, const char* instance_id, bool enable_rpc, uint64_t key_material, bool provide_extender_enabled, bool default_provide_extender, char** out_error);",
		" *   ProvideExtenderEnabled: boolean\n",
		" *   DefaultProvideExtender: boolean\n",
	} {
		if !strings.Contains(header, text) {
			t.Errorf("the header does not carry %q", text)
		}
	}

	hpp := testingGeneratedFile(t, "..", "include", "urnetwork_sdk.hpp")
	for _, text := range []string{
		"\tbool DefaultProvideExtender{};\n",
		"\tj[\"DefaultProvideExtender\"] = v.DefaultProvideExtender;\n",
		"it->get_to(v.DefaultProvideExtender);",
		"inline DeviceLocal newDeviceLocalWithProvideExtender(",
	} {
		if !strings.Contains(hpp, text) {
			t.Errorf("the c++ wrapper does not carry %q", text)
		}
	}

	// the packages that bind the header, each of which must declare the
	// constructor for its apps to call it
	for _, path := range []string{
		filepath.Join("python", "src", "urnetwork", "_raw.py"),
		filepath.Join("java", "src", "main", "java", "io", "ur", "sdk", "Raw.java"),
		filepath.Join("csharp", "Raw.g.cs"),
		filepath.Join("rust", "src", "raw.rs"),
		filepath.Join("ruby", "lib", "urnetwork", "raw.rb"),
	} {
		if !strings.Contains(testingGeneratedFile(t, "..", "..", path), cName) {
			t.Errorf("the %s binding does not declare %s", path, cName)
		}
	}
}
