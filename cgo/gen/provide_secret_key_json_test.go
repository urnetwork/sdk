// Check the real generator's narrow raw-secret JSON specialization. Executable
// byte/JSON controls live in smoke/provide_secret_key_json.cpp, not a Go copy of
// the emitted codec. All generator output stays inside test-owned directories.
package main

import (
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Resolve the real named model; the method-bearing type must remain JSON data.
func testingProvideSecretKeyType(t *testing.T, g *gen, name string) *types.Named {
	t.Helper()
	named, ok := types.Unalias(testingPreferenceType(t, g, name).Type()).(*types.Named)
	if !ok {
		t.Fatalf("SDK data model is not named: %s", name)
	}
	return named
}

func TestProvideSecretKeyBindingKeepsRawDataAndNoMethodExports(t *testing.T) {
	g := testingPreferenceGenerator(t)
	named := testingProvideSecretKeyType(t, g, "ProvideSecretKey")
	methods := types.NewMethodSet(types.NewPointer(named))
	for _, method := range []string{"MarshalJSON", "UnmarshalJSON"} {
		if methods.Lookup(nil, method) == nil {
			t.Fatalf("actual SDK JSON method missing: %s", method)
		}
	}
	for _, name := range []string{"ProvideSecretKey", "ProvideSecretKeyList"} {
		object := testingPreferenceType(t, g, name)
		if behavioralTypes[name] || g.classify(types.NewPointer(object.Type())).kind != kindJson {
			t.Fatalf("raw secret value changed C representation: %s", name)
		}
		before := len(g.exports)
		g.emitType(object)
		if len(g.exports) != before {
			t.Fatalf("JSON methods unexpectedly became C exports: %s", name)
		}
	}
	fields, listElement := g.hppStructModel(named)
	if listElement != "" || len(fields) != 2 ||
		fields[0] != (hppField{cppName: "provide_mode", jsonName: "provide_mode", cppType: "int64_t"}) ||
		fields[1] != (hppField{cppName: "provide_secret_key", jsonName: "provide_secret_key", cppType: "std::string"}) {
		t.Fatal("JSON codec changed the existing two-field raw-byte C++ value")
	}
	_, listElement = g.hppStructModel(testingProvideSecretKeyType(t, g, "ProvideSecretKeyList"))
	if listElement != "ProvideSecretKey" {
		t.Fatal("secret list stopped using the original value element")
	}
}

func TestProvideSecretKeyBindingEmitsSpecializedSerde(t *testing.T) {
	g := testingPreferenceGenerator(t)
	g.dataTypes = map[string]*types.Named{
		"ProvideSecretKey":     testingProvideSecretKeyType(t, g, "ProvideSecretKey"),
		"ProvideSecretKeyList": testingProvideSecretKeyType(t, g, "ProvideSecretKeyList"),
	}
	var output strings.Builder
	if err := g.hppDataTypes(&output); err != nil {
		t.Fatal(err)
	}
	cpp := output.String()
	for _, required := range []string{
		"struct ProvideSecretKey {\n\tint64_t provide_mode{};\n\tstd::string provide_secret_key{};\n};",
		"using ProvideSecretKeyList = std::vector<ProvideSecretKey>;",
		"inline void to_json(nlohmann::json& j, const ProvideSecretKey& v) {",
		"inline void from_json(const nlohmann::json& j, ProvideSecretKey& v) {",
		"detail::provideSecretKeyIsUtf8(v.provide_secret_key)",
		"j[\"provide_secret_key_base64\"] = detail::encodeProvideSecretKeyBase64(v.provide_secret_key);",
		"next.provide_secret_key = detail::decodeProvideSecretKeyBase64(it->get<std::string>());",
		"ProvideSecretKey next = v;", "v = std::move(next);",
	} {
		if strings.Count(cpp, required) != 1 {
			t.Fatal("raw secret specialization was omitted or duplicated")
		}
	}
	if strings.Contains(cpp, "std::string provide_secret_key_base64") ||
		strings.Contains(cpp, "it->get_to(v.provide_secret_key);") {
		t.Fatal("secret JSON bypasses the raw-field specialization")
	}
}

func TestProvideSecretKeyBindingDocumentsBinaryAlternative(t *testing.T) {
	g := testingPreferenceGenerator(t)
	doc := g.dataDoc("ProvideSecretKey", testingProvideSecretKeyType(t, g, "ProvideSecretKey"))
	for _, expected := range []string{
		"provide_mode: number (integer)",
		"provide_secret_key?: string (legacy literal UTF-8, never prefix-decoded)",
		"provide_secret_key_base64?: string (strict standard padded base64 of raw bytes)",
		"binary emits only the base64 key field",
		"Base64 rejects whitespace and noncanonical padding",
		"malformed binary never falls back",
	} {
		if !strings.Contains(doc, expected) {
			t.Fatal("C JSON-shape documentation lost the binary alternative contract")
		}
	}
	list := g.dataDoc("ProvideSecretKeyList", testingProvideSecretKeyType(t, g, "ProvideSecretKeyList"))
	if list != "/* ProvideSecretKeyList (json):\n *   = ProvideSecretKey | null[]\n */" {
		t.Fatal("list JSON shape changed instead of specializing its element")
	}
}

func TestProvideSecretKeyBindingLeavesOtherSerdeUnchanged(t *testing.T) {
	g := testingPreferenceGenerator(t)
	g.dataTypes = map[string]*types.Named{
		"ProxyConfig": testingProvideSecretKeyType(t, g, "ProxyConfig"),
	}
	var before strings.Builder
	if err := g.hppDataTypes(&before); err != nil {
		t.Fatal(err)
	}
	doc := g.dataDoc("ProxyConfig", g.dataTypes["ProxyConfig"])
	g.dataTypes["ProvideSecretKey"] = testingProvideSecretKeyType(t, g, "ProvideSecretKey")
	var after strings.Builder
	if err := g.hppDataTypes(&after); err != nil {
		t.Fatal(err)
	}
	for _, signature := range []string{
		"inline void to_json(nlohmann::json& j, const ProxyConfig& v) {\n",
		"inline void from_json(const nlohmann::json& j, ProxyConfig& v) {\n",
	} {
		_, original, found := strings.Cut(before.String(), signature)
		body, _, ended := strings.Cut(original, "\n}\n")
		if !found || !ended || !strings.Contains(after.String(), signature+body+"\n}\n") {
			t.Fatal("unrelated ordinary JSON serializer changed")
		}
	}
	if strings.Contains(doc, "provide_secret_key") || doc != g.dataDoc("ProxyConfig", g.dataTypes["ProxyConfig"]) {
		t.Fatal("binary alternative escaped its per-type documentation")
	}
}

func TestProvideSecretKeyBindingRejectsUnexpectedModel(t *testing.T) {
	g := testingPreferenceGenerator(t)
	pkg := testingProvideSecretKeyType(t, g, "ProvideSecretKey").Obj().Pkg()
	// This negative schema mutation must fail generation, never silently omit a
	// future field from the handwritten two-field JSON contract.
	changed := types.NewNamed(
		types.NewTypeName(token.NoPos, pkg, "ProvideSecretKey", nil),
		types.NewStruct([]*types.Var{
			types.NewField(token.NoPos, pkg, "ProvideSecretKey", types.Typ[types.String], false),
		}, []string{`json:"provide_secret_key"`}),
		nil,
	)
	g.dataTypes = map[string]*types.Named{"ProvideSecretKey": changed}
	var output strings.Builder
	if err := g.hppDataTypes(&output); err == nil || err.Error() != "ProvideSecretKey JSON model changed" {
		t.Fatal("changed raw secret schema was silently accepted")
	}
}

func TestProvideSecretKeyBindingGeneratedFilesKeepJsonAbi(t *testing.T) {
	g := testingPreferenceGenerator(t)
	output := t.TempDir()
	t.Chdir(output)
	if err := g.run(); err != nil {
		t.Fatal(err)
	}
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(output, path))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	header := read("include/urnetwork_sdk.h")
	cpp := read("include/urnetwork_sdk.hpp")
	exports := read("include/urnetwork_sdk.def")
	goExports := read("exports_gen.go")
	for _, declaration := range []string{
		"char* urnet_local_state_get_provide_secret_keys(uint64_t self);",
		"char* urnet_local_state_load_provide_secret_keys(uint64_t self, char** out_error);",
		"bool urnet_local_state_set_provide_secret_keys(uint64_t self, const char* provide_secret_key_list_json, char** out_error);",
		"void urnet_device_load_provide_secret_keys(uint64_t self, const char* provide_secret_key_list_json);",
	} {
		if !strings.Contains(header, declaration) {
			t.Fatal("secret JSON C signature changed")
		}
	}
	for _, symbol := range []string{
		"urnet_provide_secret_key_marshal_json", "urnet_provide_secret_key_unmarshal_json",
	} {
		if strings.Contains(header, symbol) || strings.Contains(exports, symbol) || strings.Contains(goExports, symbol) {
			t.Fatal("JSON implementation method leaked into public C exports")
		}
	}
	if !strings.Contains(header, "provide_secret_key_base64?: string (strict standard padded base64 of raw bytes)") ||
		!strings.Contains(cpp, hppProvideSecretKeyJson) ||
		!strings.Contains(cpp, "std::string provide_secret_key{};") ||
		!strings.Contains(cpp, "nlohmann::json(*provide_secret_key_list).dump()") ||
		!strings.Contains(cpp, "detail::parseJson<ProvideSecretKeyList>") ||
		!strings.Contains(goExports, "cJson(r0, \"urnet_local_state_load_provide_secret_keys\")") {
		t.Fatal("fresh C/C++/Go JSON boundary lost its actual specialized value path")
	}
}
