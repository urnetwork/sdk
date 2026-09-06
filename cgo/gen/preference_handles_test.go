// Exercise the actual generator against the SDK type package. Generated files
// are written only beneath each test's temporary directory, never over sources.
package main

import (
	"go/types"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The production generator is a single invocation with package-level caches.
// Keep these tests serial and restore both caches after each invocation.
func testingPreferenceGenerator(t *testing.T) *gen {
	t.Helper()
	previousCallbacks := callbackCache
	previousData := recordedData
	callbackCache = map[string]*callbackInfo{}
	recordedData = map[string]bool{}
	t.Cleanup(func() {
		callbackCache = previousCallbacks
		recordedData = previousData
	})
	g, err := load()
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// Resolve the real SDK declaration rather than a test lookalike.
func testingPreferenceType(t *testing.T, g *gen, name string) *types.TypeName {
	t.Helper()
	object, ok := g.scope.Lookup(name).(*types.TypeName)
	if !ok {
		t.Fatalf("SDK type %s is unavailable", name)
	}
	return object
}

// Find one emitted production call, including its C declaration and Go body.
func testingPreferenceExport(t *testing.T, g *gen, receiver, method string) *export {
	t.Helper()
	var found *export
	for _, item := range g.exports {
		if item.sig.recvName == receiver && item.sig.goName == method {
			if found != nil {
				t.Fatalf("duplicate binding %s.%s", receiver, method)
			}
			found = item
		}
	}
	if found == nil {
		t.Fatalf("private observation getter was not emitted: %s.%s", receiver, method)
	}
	return found
}

func TestPreferenceBindingObjectsUseOpaqueHandles(t *testing.T) {
	g := testingPreferenceGenerator(t)
	for _, name := range []string{
		"LocalAuthStateSnapshot", "LocalStateResetResult",
		"DeviceLocalLoadResult", "DeviceLocalSaveResult",
		"LocalStateLocationReadResult", "LocalStateKeyMaterialReadResult",
	} {
		object := testingPreferenceType(t, g, name)
		info := g.classify(types.NewPointer(object.Type()))
		if info.kind != kindHandle || info.named == nil || info.named.Obj().Name() != name {
			t.Fatalf("private observation must cross as an opaque handle: %s (kind=%d)", name, info.kind)
		}
	}
}

func TestPreferenceBindingEmitsAllObservationGetters(t *testing.T) {
	g := testingPreferenceGenerator(t)
	for _, name := range []string{
		"LocalAuthStateSnapshot", "LocalStateResetResult",
		"DeviceLocalLoadResult", "DeviceLocalSaveResult",
		"LocalStateLocationReadResult", "LocalStateKeyMaterialReadResult",
	} {
		g.emitType(testingPreferenceType(t, g, name))
	}
	for _, expected := range []struct {
		receiver string
		methods  []string
	}{
		{receiver: "LocalAuthStateSnapshot", methods: []string{
			"GetEmpty", "GetInstanceId", "GetByJwt", "GetByClientJwt", "ParseByJwt",
			"LoadConnectLocation", "LoadDefaultLocation", "SetConnectLocation", "SetDefaultLocation",
		}},
		{receiver: "LocalStateResetResult", methods: []string{"GetReset", "GetDeviceLocalKeyMaterial"}},
		{receiver: "LocalStateLocationReadResult", methods: []string{"GetLocation"}},
		{receiver: "LocalStateKeyMaterialReadResult", methods: []string{"GetKeyMaterial"}},
		{receiver: "DeviceLocalLoadResult", methods: []string{
			"GetLoaded", "GetHasConnectLocation", "GetHasDefaultLocation", "GetDefaultError",
			"GetHasPreference", "GetPreferenceError",
		}},
		{receiver: "DeviceLocalSaveResult", methods: []string{
			"GetSequence", "GetPreference", "GetAutoSaveEnabled", "GetSaved", "GetError",
		}},
	} {
		for _, method := range expected.methods {
			item := testingPreferenceExport(t, g, expected.receiver, method)
			if !strings.Contains(item.cDecl, "uint64_t self") ||
				!strings.Contains(item.goCode, "resolveHandle[*sdk."+expected.receiver+"]") ||
				!strings.Contains(item.goCode, "self_."+method+"(") {
				t.Fatalf("getter lost its original object handle: %s.%s", expected.receiver, method)
			}
		}
	}
	key := testingPreferenceExport(t, g, "LocalStateResetResult", "GetDeviceLocalKeyMaterial")
	if key.sig.result == nil || key.sig.result.kind != kindHandle ||
		key.sig.result.named.Obj().Name() != "DeviceLocalKeyMaterial" {
		t.Fatal("reset result lost the actual preserved key handle")
	}
}

func TestPreferenceBindingCheckedOperationsKeepHandleAndError(t *testing.T) {
	g := testingPreferenceGenerator(t)
	for _, name := range []string{"NetworkSpace", "LocalState", "LocalAuthStateSnapshot", "DeviceLocal"} {
		g.emitType(testingPreferenceType(t, g, name))
	}
	for _, expected := range []struct {
		receiver string
		method   string
		result   string
		hasError bool
	}{
		{receiver: "NetworkSpace", method: "GetAuthStateSnapshot", result: "LocalAuthStateSnapshot", hasError: true},
		{receiver: "LocalState", method: "GetAuthStateSnapshot", result: "LocalAuthStateSnapshot", hasError: true},
		{receiver: "NetworkSpace", method: "ResetLocalStateIfCurrent", result: "LocalStateResetResult", hasError: true},
		{receiver: "DeviceLocal", method: "Load", result: "DeviceLocalLoadResult", hasError: true},
		{receiver: "DeviceLocal", method: "GetLastLocalStateSaveResult", result: "DeviceLocalSaveResult"},
		{receiver: "LocalState", method: "ReadConnectLocation", result: "LocalStateLocationReadResult", hasError: true},
		{receiver: "LocalState", method: "ReadDefaultLocation", result: "LocalStateLocationReadResult", hasError: true},
		{receiver: "LocalAuthStateSnapshot", method: "ReadConnectLocation", result: "LocalStateLocationReadResult", hasError: true},
		{receiver: "LocalAuthStateSnapshot", method: "ReadDefaultLocation", result: "LocalStateLocationReadResult", hasError: true},
		{receiver: "LocalState", method: "ReadDeviceLocalKeyMaterial", result: "LocalStateKeyMaterialReadResult", hasError: true},
	} {
		item := testingPreferenceExport(t, g, expected.receiver, expected.method)
		if item.sig.result == nil || item.sig.result.kind != kindHandle ||
			item.sig.result.named.Obj().Name() != expected.result || item.sig.hasError != expected.hasError {
			t.Fatalf("checked observation lost its result/error contract: %s.%s", expected.receiver, expected.method)
		}
		if !strings.HasPrefix(item.cDecl, "uint64_t ") ||
			!strings.Contains(item.goCode, "if r0 == nil {") ||
			!strings.Contains(item.goCode, "newHandle(r0)") {
			t.Fatalf("nullable result is not an owned handle: %s.%s", expected.receiver, expected.method)
		}
		if expected.hasError && !strings.Contains(item.cDecl, "char** out_error") {
			t.Fatalf("checked observation lost C error output: %s.%s", expected.receiver, expected.method)
		}
	}
	reset := testingPreferenceExport(t, g, "NetworkSpace", "ResetLocalStateIfCurrent")
	if len(reset.sig.params) != 1 || reset.sig.params[0].info.kind != kindHandle ||
		reset.sig.params[0].info.named.Obj().Name() != "LocalAuthStateSnapshot" ||
		!strings.Contains(reset.goCode, "resolveHandle[*sdk.LocalAuthStateSnapshot]") {
		t.Fatal("conditional reset no longer receives the original snapshot handle")
	}
	for _, method := range []string{
		"SetAutoSave", "SetConnectLocationChecked", "SetDefaultLocationChecked", "ReconnectChecked",
		"SaveKeyMaterial", "SaveProvideSecretKeys",
	} {
		item := testingPreferenceExport(t, g, "DeviceLocal", method)
		if item.sig.result != nil || !item.sig.hasError ||
			!strings.HasPrefix(item.cDecl, "bool ") || !strings.Contains(item.cDecl, "char** out_error") {
			t.Fatalf("checked mutation lost C success/error output: %s", method)
		}
	}
	for _, method := range []string{"SaveKeyMaterial", "SaveProvideSecretKeys"} {
		item := testingPreferenceExport(t, g, "DeviceLocal", method)
		if len(item.sig.params) != 0 ||
			!strings.Contains(item.goCode, "resolveHandle[*sdk.DeviceLocal]") ||
			!strings.Contains(item.goCode, "err := self_."+method+"()\n\tif err != nil {\n\t\tsetErrorOut(outError, err)\n\t\treturn C.bool(false)\n\t}\n\treturn C.bool(true)") {
			t.Fatalf("explicit key save lost its operation-bound error dispatch: %s", method)
		}
	}
	secrets := testingPreferenceExport(t, g, "LocalState", "LoadProvideSecretKeys")
	if len(secrets.sig.params) != 0 || secrets.sig.result == nil ||
		secrets.sig.result.kind != kindJson || !secrets.sig.result.pointer ||
		secrets.sig.result.named == nil || secrets.sig.result.named.Obj().Name() != "ProvideSecretKeyList" ||
		!secrets.sig.hasError ||
		secrets.cDecl != "char* urnet_local_state_load_provide_secret_keys(uint64_t self, char** out_error);" {
		t.Fatal("checked secret load lost its nullable legacy list/error contract")
	}
	if !strings.Contains(secrets.goCode, "resolveHandle[*sdk.LocalState]") ||
		!strings.Contains(secrets.goCode, "r0, err := self_.LoadProvideSecretKeys()\n\tif err != nil {\n\t\tsetErrorOut(outError, err)\n\t\treturn nil\n\t}\n\tif r0 == nil {\n\t\treturn nil\n\t}\n\treturn cJson(r0, \"urnet_local_state_load_provide_secret_keys\")") ||
		strings.Contains(secrets.goCode, "newHandle(r0)") {
		t.Fatal("checked secret load conflated an error, an absent pointer, or a present JSON list")
	}
}

func TestPreferenceBindingSaveCallbackTransfersOwnedResultHandle(t *testing.T) {
	g := testingPreferenceGenerator(t)
	object := testingPreferenceType(t, g, "LocalStateSaveListener")
	named, ok := types.Unalias(object.Type()).(*types.Named)
	if !ok {
		t.Fatal("save listener is not a named SDK interface")
	}
	callback, err := g.callback(named)
	if err != nil {
		t.Fatal(err)
	}
	if len(callback.methods) != 1 || callback.methods[0].name != "LocalStateSaved" {
		t.Fatal("save listener shape changed")
	}
	method := callback.methods[0]
	if len(method.params) != 1 || method.params[0].info.kind != kindHandle ||
		method.params[0].info.named.Obj().Name() != "DeviceLocalSaveResult" ||
		!strings.Contains(method.typedef, "uint64_t result") ||
		!strings.Contains(method.adapter, "newHandle(result)") ||
		strings.Contains(method.adapter, "cJson(result") || strings.Contains(method.typedef, "result_json") {
		t.Fatal("save callback serialized away its immutable operation result")
	}
}

func TestPreferenceBindingLegacyDataAndKeyContractsRemain(t *testing.T) {
	g := testingPreferenceGenerator(t)
	for _, name := range []string{"ConnectLocation", "ByJwt", "ProvideSecretKeyList"} {
		info := g.classify(types.NewPointer(testingPreferenceType(t, g, name).Type()))
		if info.kind != kindJson {
			t.Fatalf("existing value type no longer crosses as JSON: %s", name)
		}
	}
	for _, name := range []string{"DeviceLocalKeyMaterial", "DeviceRpcKeyMaterial", "NetworkSpace"} {
		info := g.classify(types.NewPointer(testingPreferenceType(t, g, name).Type()))
		if info.kind != kindHandle {
			t.Fatalf("existing owned handle changed representation: %s", name)
		}
	}
	g.emitType(testingPreferenceType(t, g, "DeviceLocalKeyMaterial"))
	empty := testingPreferenceExport(t, g, "DeviceLocalKeyMaterial", "IsEmpty")
	if empty.sig.result == nil || empty.sig.result.kind != kindBool || empty.sig.hasError {
		t.Fatal("existing key IsEmpty contract changed")
	}
	for _, name := range []string{"Device", "LocalState"} {
		g.emitType(testingPreferenceType(t, g, name))
	}
	for _, expected := range []struct {
		receiver    string
		method      string
		declaration string
	}{
		{receiver: "Device", method: "Reconnect", declaration: "void urnet_device_reconnect(uint64_t self, const char* location_json);"},
		{receiver: "Device", method: "SetConnectLocation", declaration: "void urnet_device_set_connect_location(uint64_t self, const char* location_json);"},
		{receiver: "Device", method: "SetDefaultLocation", declaration: "void urnet_device_set_default_location(uint64_t self, const char* location_json);"},
		{receiver: "LocalState", method: "GetConnectLocation", declaration: "char* urnet_local_state_get_connect_location(uint64_t self);"},
		{receiver: "LocalState", method: "GetDefaultLocation", declaration: "char* urnet_local_state_get_default_location(uint64_t self);"},
		{receiver: "LocalState", method: "SetConnectLocation", declaration: "bool urnet_local_state_set_connect_location(uint64_t self, const char* connect_location_json, char** out_error);"},
		{receiver: "LocalState", method: "SetDefaultLocation", declaration: "bool urnet_local_state_set_default_location(uint64_t self, const char* connect_location_json, char** out_error);"},
		{receiver: "LocalState", method: "GetProvideSecretKeys", declaration: "char* urnet_local_state_get_provide_secret_keys(uint64_t self);"},
		{receiver: "LocalState", method: "SetProvideSecretKeys", declaration: "bool urnet_local_state_set_provide_secret_keys(uint64_t self, const char* provide_secret_key_list_json, char** out_error);"},
		{receiver: "Device", method: "LoadProvideSecretKeys", declaration: "void urnet_device_load_provide_secret_keys(uint64_t self, const char* provide_secret_key_list_json);"},
	} {
		if actual := testingPreferenceExport(t, g, expected.receiver, expected.method).cDecl; actual != expected.declaration {
			t.Fatalf("legacy C signature changed: %s.%s", expected.receiver, expected.method)
		}
	}
}

func TestPreferenceBindingGeneratedFilesCarryGettersAndAdditiveAbi(t *testing.T) {
	g := testingPreferenceGenerator(t)
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("test source path unavailable")
	}
	baseline, err := os.ReadFile(filepath.Join(filepath.Dir(source), "testdata", "exported_symbols.txt"))
	if err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	t.Chdir(output)
	if err := g.run(); err != nil {
		t.Fatal(err)
	}
	read := func(path string) string {
		t.Helper()
		bytes, err := os.ReadFile(filepath.Join(output, path))
		if err != nil {
			t.Fatal(err)
		}
		return string(bytes)
	}
	header := read("include/urnetwork_sdk.h")
	cpp := read("include/urnetwork_sdk.hpp")
	exports := "\n" + read("include/urnetwork_sdk.def") + "\n"
	goExports := read("exports_gen.go")
	for _, name := range []string{
		"LocalAuthStateSnapshot", "LocalStateResetResult",
		"DeviceLocalLoadResult", "DeviceLocalSaveResult",
		"LocalStateLocationReadResult", "LocalStateKeyMaterialReadResult",
	} {
		if !strings.Contains(cpp, "class "+name+" final : public detail::Handle") ||
			strings.Contains(cpp, "struct "+name+" {") {
			t.Fatalf("generated C++ erased private observation getters: %s", name)
		}
	}
	for _, declaration := range []string{
		"DeviceLocalLoadResult load() const;",
		"LocalStateLocationReadResult readConnectLocation() const;",
		"LocalStateLocationReadResult readDefaultLocation() const;",
		"LocalStateKeyMaterialReadResult readDeviceLocalKeyMaterial() const;",
		"DeviceLocalSaveResult getLastLocalStateSaveResult() const;",
		"LocalStateResetResult resetLocalStateIfCurrent(const LocalAuthStateSnapshot& snapshot) const;",
		"using LocalStateSaveListener = std::function<void(DeviceLocalSaveResult result)>;",
		"std::string getDefaultError() const;", "std::string getPreferenceError(const std::string& name) const;",
		"void saveKeyMaterial() const;", "void saveProvideSecretKeys() const;",
		"std::optional<ProvideSecretKeyList> loadProvideSecretKeys() const;",
		"using ProvideSecretKeyList = std::vector<ProvideSecretKey>;",
	} {
		if !strings.Contains(cpp, declaration) {
			t.Fatalf("generated C++ observation contract missing: %s", declaration)
		}
	}
	if !strings.Contains(header, "uint64_t urnet_device_local_load(uint64_t self, char** out_error);") ||
		!strings.Contains(header, "uint64_t urnet_network_space_get_auth_state_snapshot(uint64_t self, char** out_error);") ||
		!strings.Contains(header, "uint64_t urnet_network_space_reset_local_state_if_current(uint64_t self, uint64_t snapshot, char** out_error);") ||
		!strings.Contains(header, "bool urnet_device_local_save_key_material(uint64_t self, char** out_error);") ||
		!strings.Contains(header, "bool urnet_device_local_save_provide_secret_keys(uint64_t self, char** out_error);") ||
		!strings.Contains(header, "char* urnet_local_state_load_provide_secret_keys(uint64_t self, char** out_error);") ||
		!strings.Contains(goExports, "resolveHandle[*sdk.DeviceLocalLoadResult]") ||
		!strings.Contains(goExports, "resolveHandle[*sdk.DeviceLocalSaveResult]") {
		t.Fatal("generated C/Go boundary lost checked handles or getter dispatch")
	}
	for _, expected := range []struct {
		signature string
		call      string
	}{
		{signature: "inline void DeviceLocal::saveKeyMaterial() const", call: "urnet_device_local_save_key_material"},
		{signature: "inline void DeviceLocal::saveProvideSecretKeys() const", call: "urnet_device_local_save_provide_secret_keys"},
	} {
		_, tail, found := strings.Cut(cpp, expected.signature+" {\n")
		body, _, ended := strings.Cut(tail, "\n}\n")
		if !found || !ended ||
			!strings.Contains(body, "bool ok = "+expected.call+"(handle(), &err_c);") ||
			!strings.Contains(body, "if (err_c) {\n\t\tdetail::throwError(err_c);\n\t}") ||
			!strings.Contains(body, "if (!ok) {\n\t\tthrow Error(\"urnet: "+expected.call+" failed\");\n\t}") {
			t.Fatal("generated C++ explicit key save discarded operation failure")
		}
	}
	_, secretTail, found := strings.Cut(cpp, "inline std::optional<ProvideSecretKeyList> LocalState::loadProvideSecretKeys() const {\n")
	secretBody, _, ended := strings.Cut(secretTail, "\n}\n")
	if !found || !ended ||
		!strings.Contains(secretBody, "char* r_c = urnet_local_state_load_provide_secret_keys(handle(), &err_c);\n\tif (err_c) {\n\t\tdetail::throwError(err_c);\n\t}\n\tauto r_s = detail::takeStringOpt(r_c);\n\tif (!r_s) {\n\t\treturn std::nullopt;\n\t}\n\treturn detail::parseJson<ProvideSecretKeyList>(r_s->c_str());") ||
		!strings.Contains(cpp, "if (j.is_null()) {\n\t\t\treturn T{};\n\t\t}") {
		t.Fatal("generated C++ checked secret load conflated error, absence, or legacy empty list")
	}
	for _, symbol := range []string{
		"urnet_device_local_save_key_material", "urnet_device_local_save_provide_secret_keys",
		"urnet_local_state_load_provide_secret_keys",
	} {
		if !strings.Contains(exports, "\n\t"+symbol+"\n") {
			t.Fatal("fresh generation omitted an additive checked key export")
		}
	}
	for _, line := range strings.Split(string(baseline), "\n") {
		symbol := strings.TrimSpace(line)
		if symbol == "" || strings.HasPrefix(symbol, "#") {
			continue
		}
		if !strings.Contains(exports, "\n\t"+symbol+"\n") {
			t.Errorf("fresh generation removed compatibility export %s", symbol)
		}
	}
}
