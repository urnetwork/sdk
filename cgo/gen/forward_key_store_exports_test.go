package main

// The VPN component's forwarding key store crosses the c abi (QD-844; sidecar §18.6's Setup, owner decision 197,
// SDK-M8-16, SDK-M8-06's tables): DeviceLocalSettings crosses as json, which carries no key store, so the Windows and
// Linux app services hand theirs to NewDeviceLocalWithForwardKeyStore, where SidecarForwardKeyStore crosses as three
// function pointers and a user_data, as SidecarKeyStore does on NewSidecarLocal. Until this constructor the callback
// type was in the coverage report with no c function that took it.

import (
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const forwardKeyStoreConstructorCName = "urnet_new_device_local_with_forward_key_store"

// The generator maps the constructor: NewDeviceLocal's parameters with the settings decoded over their defaults, then
// the key store as one callback parameter of its three methods, which the export hands to the sdk as an adapter over
// all three functions and the user_data, and as no store at all where the host passed none.
func TestForwardKeyStoreConstructorIsMapped(t *testing.T) {
	g := testingPreferenceGenerator(t)
	function, ok := g.scope.Lookup("NewDeviceLocalWithForwardKeyStore").(*types.Func)
	if !ok {
		t.Fatal("the sdk declares no NewDeviceLocalWithForwardKeyStore")
	}
	g.emitFunc(function)
	for _, skipped := range g.skipped {
		if skipped.symbol == "NewDeviceLocalWithForwardKeyStore" {
			t.Fatalf("the constructor is skipped: %s", skipped.reason)
		}
	}
	constructor := testingPreferenceExport(t, g, "", "NewDeviceLocalWithForwardKeyStore")
	if constructor.cName != forwardKeyStoreConstructorCName {
		t.Errorf("the constructor is exported as %s", constructor.cName)
	}
	if constructor.unixOnly {
		t.Error("the constructor is exported on unix only, and the Windows app service binds it")
	}
	if !constructor.sig.hasError || constructor.sig.result == nil || constructor.sig.result.kind != kindHandle {
		t.Error("the constructor does not answer a device handle and an error")
	}

	wantKinds := []kind{kindHandle, kindString, kindString, kindString, kindString, kindId, kindJson, kindCallback}
	if len(constructor.sig.params) != len(wantKinds) {
		t.Fatalf("the constructor takes %d parameters", len(constructor.sig.params))
	}
	for i, want := range wantKinds {
		if got := constructor.sig.params[i].info.kind; got != want {
			t.Errorf("parameter %s crosses as kind %d, expected %d", constructor.sig.params[i].name, got, want)
		}
	}
	keyStore := constructor.sig.params[len(wantKinds)-1]
	if keyStore.cb == nil || keyStore.cb.ifaceName != "SidecarForwardKeyStore" {
		t.Fatalf("the last parameter is not the forwarding key store: %+v", keyStore.cb)
	}
	methods := []string{}
	for _, method := range keyStore.cb.methods {
		methods = append(methods, method.name)
	}
	if strings.Join(methods, " ") != "DeleteSecret GetSecret PutSecret" {
		t.Errorf("the key store crosses with the functions %v", methods)
	}

	for _, text := range []string{
		"settings_ := sdk.DefaultDeviceLocalSettings()",
		"var forwardKeyStore_ sdk.SidecarForwardKeyStore\n",
		"forwardKeyStore_ = &cAdapterSidecarForwardKeyStore{cbDeleteSecret: forwardKeyStore_delete_secret, cbGetSecret: forwardKeyStore_get_secret, cbPutSecret: forwardKeyStore_put_secret, userData: forwardKeyStore_user_data}",
		"settings_, forwardKeyStore_)",
	} {
		if !strings.Contains(constructor.goCode, text) {
			t.Errorf("the export does not carry %q:\n%s", text, constructor.goCode)
		}
	}
}

// The tracked surfaces list the constructor: the coverage report, the module definition, the header's prototype and
// the three function pointer types it takes, the internal shims, the c++ wrapper with the store as a struct of three
// functions, and the five language bindings that bind the header.
func TestForwardKeyStoreConstructorCrossesTheAbi(t *testing.T) {
	const cName = forwardKeyStoreConstructorCName

	exportedCNames, accounted := testingCoverageReport(t)
	if exported := exportedCNames["NewDeviceLocalWithForwardKeyStore"]; exported != cName {
		t.Errorf("NewDeviceLocalWithForwardKeyStore is exported as %q, expected %s", exported, cName)
	}
	for _, identifier := range []string{"SidecarForwardKeyStore", "SidecarForwardSecret"} {
		if !accounted[identifier] {
			t.Errorf("%s is not in the coverage report", identifier)
		}
	}
	if !testingDefSymbols(t)[cName] {
		t.Errorf("the module definition does not name %s", cName)
	}

	header := testingGeneratedFile(t, "..", "include", "urnetwork_sdk.h")
	for _, text := range []string{
		"\nuint64_t urnet_new_device_local_with_forward_key_store(uint64_t network_space, const char* by_jwt, const char* device_description, const char* device_spec, const char* app_version, const char* instance_id, const char* settings_json, urnet_sidecar_forward_key_store_delete_secret_cb forward_key_store_delete_secret, urnet_sidecar_forward_key_store_get_secret_cb forward_key_store_get_secret, urnet_sidecar_forward_key_store_put_secret_cb forward_key_store_put_secret, void* forward_key_store_user_data, char** out_error);\n",
		"/* SidecarForwardKeyStore */\n",
		"typedef int64_t (*urnet_sidecar_forward_key_store_delete_secret_cb)(void* user_data, const char* name);\n",
		"typedef char* (*urnet_sidecar_forward_key_store_get_secret_cb)(void* user_data, const char* name);\n",
		"typedef int64_t (*urnet_sidecar_forward_key_store_put_secret_cb)(void* user_data, const char* name, const char* value_base64);\n",
		"/* SidecarForwardSecret (json):\n *   status: number\n *   value_base64?: string\n */\n",
		// the component's directory is the settings json's to say
		" *   ForwardStateDir: string\n",
	} {
		if !strings.Contains(header, text) {
			t.Errorf("the header does not carry %q", text)
		}
	}

	callbacks := testingGeneratedFile(t, "..", "callbacks.h")
	for _, shim := range []string{
		"urnet_invoke_sidecar_forward_key_store_delete_secret(",
		"urnet_invoke_sidecar_forward_key_store_get_secret(",
		"urnet_invoke_sidecar_forward_key_store_put_secret(",
	} {
		if !strings.Contains(callbacks, shim) {
			t.Errorf("callbacks.h does not declare %s", shim)
		}
	}

	hpp := testingGeneratedFile(t, "..", "include", "urnetwork_sdk.hpp")
	for _, text := range []string{
		"struct SidecarForwardKeyStore {\n\tstd::function<int64_t(std::string name)> delete_secret;\n\tstd::function<std::optional<SidecarForwardSecret>(std::string name)> get_secret;\n\tstd::function<int64_t(std::string name, std::string value_base64)> put_secret;\n};\n",
		"inline DeviceLocal newDeviceLocalWithForwardKeyStore(const NetworkSpace& network_space, const std::string& by_jwt, const std::string& device_description, const std::string& device_spec, const std::string& app_version, const std::string& instance_id, const std::optional<DeviceLocalSettings>& settings, SidecarForwardKeyStore forward_key_store) {\n",
		// the device keeps the store's functions alive as long as it lives
		"\tif (forward_key_store_fn) {\n\t\tr.retain(forward_key_store_fn);\n\t}\n",
	} {
		if !strings.Contains(hpp, text) {
			t.Errorf("the c++ wrapper does not carry %q", text)
		}
	}

	for _, path := range []string{
		filepath.Join("python", "src", "urnetwork", "_raw.py"),
		filepath.Join("java", "src", "main", "java", "io", "ur", "sdk", "Raw.java"),
		filepath.Join("csharp", "Raw.g.cs"),
		filepath.Join("rust", "src", "raw.rs"),
		filepath.Join("ruby", "lib", "urnetwork", "raw.rb"),
	} {
		binding := testingGeneratedFile(t, "..", "..", path)
		for _, text := range []string{cName, "urnet_sidecar_forward_key_store_get_secret_cb"} {
			if !strings.Contains(binding, text) {
				t.Errorf("the %s binding does not declare %s", path, text)
			}
		}
	}
}

// A C host's call compiles against the tracked header as C: three functions of the header's callback types, the
// header's status constants, a get answered through urnet_copy_string, and the constructor taking them.
func TestForwardKeyStoreConstructorCompilesForACHost(t *testing.T) {
	compiler, err := exec.LookPath("clang")
	if err != nil {
		t.Skip("clang is not installed")
	}
	program := filepath.Join(t.TempDir(), "forward_key_store_host.c")
	if err := os.WriteFile(program, []byte(forwardKeyStoreHostProgram), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command(compiler, "-std=c11", "-Wall", "-Werror", "-fsyntax-only",
		"-I"+filepath.Join(testingGenDir(t), "..", "include"), program)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("a c host's call does not compile: %v\n%s", err, output)
	}
}

// A desktop service's forwarding key store, as little of one as compiles: every item reads absent.
const forwardKeyStoreHostProgram = `#include <stddef.h>

#include "urnetwork_sdk.h"

static int64_t put_secret(void* user_data, const char* name, const char* value_base64) {
	(void)user_data;
	(void)name;
	(void)value_base64;
	return URNET_SIDECAR_FORWARD_KEY_STORE_LOCKED;
}

static char* get_secret(void* user_data, const char* name) {
	(void)user_data;
	(void)name;
	return urnet_copy_string("{\"status\":3}");
}

static int64_t delete_secret(void* user_data, const char* name) {
	(void)user_data;
	(void)name;
	return URNET_SIDECAR_FORWARD_KEY_STORE_OK;
}

uint64_t new_device(uint64_t network_space, const char* by_jwt, const char* instance_id, const char* settings_json, void* store, char** out_error) {
	urnet_sidecar_forward_key_store_put_secret_cb put = put_secret;
	urnet_sidecar_forward_key_store_get_secret_cb get = get_secret;
	urnet_sidecar_forward_key_store_delete_secret_cb del = delete_secret;
	return urnet_new_device_local_with_forward_key_store(network_space, by_jwt, "", "", "", instance_id, settings_json, del, get, put, store, out_error);
}
`
