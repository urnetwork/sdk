// emits include/urnetwork_sdk.hpp: a header-only c++17 wrapper over the c abi.
//
//   - raii: behavioral handles release on destruct (Sub also closes)
//   - data types are real structs with nlohmann-json to_json/from_json
//   - callbacks are std::function, trampolined through user_data
//   - functions with a go error throw urnet::Error
//
// requires nlohmann/json (header-only; brew install nlohmann-json).
package main

import (
	"fmt"
	"go/types"
	"os"
	"sort"
	"strings"
)

var cppReserved = map[string]bool{
	"alignas": true, "alignof": true, "auto": true, "bool": true, "break": true,
	"case": true, "catch": true, "char": true, "class": true, "const": true,
	"continue": true, "default": true, "delete": true, "do": true, "double": true,
	"else": true, "enum": true, "explicit": true, "export": true, "extern": true,
	"false": true, "float": true, "for": true, "friend": true, "goto": true,
	"if": true, "inline": true, "int": true, "long": true, "namespace": true,
	"new": true, "nullptr": true, "operator": true, "private": true, "protected": true,
	"public": true, "register": true, "return": true, "short": true, "signed": true,
	"sizeof": true, "static": true, "struct": true, "switch": true, "template": true,
	"this": true, "throw": true, "true": true, "try": true, "typedef": true,
	"typeid": true, "typename": true, "union": true, "unsigned": true, "using": true,
	"virtual": true, "void": true, "volatile": true, "while": true,
}

func cppName(name string) string {
	if cppReserved[name] {
		return name + "_"
	}
	return name
}

func lowerCamel(name string) string {
	if name == "" {
		return name
	}
	return cppName(strings.ToLower(name[:1]) + name[1:])
}

// hand-written class members (see exports_manual.go); keep in sync
var hppExtraClassDecls = map[string]string{
	"DeviceLocal": `	/* stable provider identity across process starts */
	std::vector<uint8_t> getClientKeySeed() const;
	std::vector<uint8_t> getProvideTlsCertificatePem() const;
	std::vector<uint8_t> getProvideTlsPrivateKeyPem() const;
	/* the raw public identity key (post quantum identity) */
	std::vector<uint8_t> getPublicIdentityKey() const;
`,
	"DeviceRemote": `	/* the raw public identity key (post quantum identity) */
	std::vector<uint8_t> getPublicIdentityKey() const;
`,
	"DeviceLocalKeyMaterial": `	std::vector<uint8_t> getClientKeySeed() const;
	std::vector<uint8_t> getProvideTlsCertificatePem() const;
	std::vector<uint8_t> getProvideTlsPrivateKeyPem() const;
`,
}

const hppExtraDefinitions = `/* ----- hand-written wrappers (buffer-out c functions) ----- */

namespace detail {
template <typename F>
inline std::vector<uint8_t> bufferOut(F f) {
	int32_t len = 0;
	f(nullptr, &len);
	if (len <= 0) {
		return {};
	}
	std::vector<uint8_t> out((size_t)len);
	if (!f(out.data(), &len)) {
		return {};
	}
	out.resize((size_t)len);
	return out;
}
} // namespace detail

inline std::vector<uint8_t> DeviceLocal::getClientKeySeed() const {
	return detail::bufferOut([h = handle()](uint8_t* out, int32_t* len) { return urnet_device_local_get_client_key_seed(h, out, len); });
}
inline std::vector<uint8_t> DeviceLocal::getProvideTlsCertificatePem() const {
	return detail::bufferOut([h = handle()](uint8_t* out, int32_t* len) { return urnet_device_local_get_provide_tls_certificate_pem(h, out, len); });
}
inline std::vector<uint8_t> DeviceLocal::getProvideTlsPrivateKeyPem() const {
	return detail::bufferOut([h = handle()](uint8_t* out, int32_t* len) { return urnet_device_local_get_provide_tls_private_key_pem(h, out, len); });
}
inline std::vector<uint8_t> DeviceLocalKeyMaterial::getClientKeySeed() const {
	return detail::bufferOut([h = handle()](uint8_t* out, int32_t* len) { return urnet_device_local_key_material_get_client_key_seed(h, out, len); });
}
inline std::vector<uint8_t> DeviceLocalKeyMaterial::getProvideTlsCertificatePem() const {
	return detail::bufferOut([h = handle()](uint8_t* out, int32_t* len) { return urnet_device_local_key_material_get_provide_tls_certificate_pem(h, out, len); });
}
inline std::vector<uint8_t> DeviceLocalKeyMaterial::getProvideTlsPrivateKeyPem() const {
	return detail::bufferOut([h = handle()](uint8_t* out, int32_t* len) { return urnet_device_local_key_material_get_provide_tls_private_key_pem(h, out, len); });
}
inline std::vector<uint8_t> DeviceLocal::getPublicIdentityKey() const {
	return detail::bufferOut([h = handle()](uint8_t* out, int32_t* len) { return urnet_device_get_public_identity_key(h, out, len); });
}
inline std::vector<uint8_t> DeviceRemote::getPublicIdentityKey() const {
	return detail::bufferOut([h = handle()](uint8_t* out, int32_t* len) { return urnet_device_get_public_identity_key(h, out, len); });
}

/* the canonical identicon png for arbitrary input bytes (identity keys):
 * deterministic and byte-identical on every platform for the same input */
inline std::vector<uint8_t> renderIdenticonPng(const std::vector<uint8_t>& input, int32_t size) {
	char* err = nullptr;
	int32_t len = 0;
	urnet_render_identicon_png(input.data(), (int32_t)input.size(), size, nullptr, &len, &err);
	if (err) {
		detail::throwError(err);
	}
	std::vector<uint8_t> out((size_t)len);
	if (0 < len) {
		if (!urnet_render_identicon_png(input.data(), (int32_t)input.size(), size, out.data(), &len, &err)) {
			detail::throwError(err);
		}
	}
	out.resize((size_t)len);
	return out;
}

/* decode a base58 string; std::nullopt when invalid */
inline std::optional<std::vector<uint8_t>> decodeBase58(const std::string& s) {
	int32_t len = 0;
	urnet_decode_base58(s.c_str(), nullptr, &len);
	if (len <= 0) {
		return std::nullopt;
	}
	std::vector<uint8_t> out((size_t)len);
	if (!urnet_decode_base58(s.c_str(), out.data(), &len)) {
		return std::nullopt;
	}
	out.resize((size_t)len);
	return out;
}

inline std::vector<uint8_t> decryptData(const std::string& encryptedDataBase58, const std::string& nonceBase58, const std::string& sharedSecretBase58) {
	char* err = nullptr;
	int32_t len = 0;
	urnet_decrypt_data(encryptedDataBase58.c_str(), nonceBase58.c_str(), sharedSecretBase58.c_str(), nullptr, &len, &err);
	if (err) {
		detail::throwError(err);
	}
	std::vector<uint8_t> out((size_t)len);
	if (0 < len) {
		if (!urnet_decrypt_data(encryptedDataBase58.c_str(), nonceBase58.c_str(), sharedSecretBase58.c_str(), out.data(), &len, &err)) {
			detail::throwError(err);
		}
	}
	out.resize((size_t)len);
	return out;
}

inline std::vector<uint8_t> generateSharedSecret(const std::vector<uint8_t>& privateKey, const std::vector<uint8_t>& publicKey) {
	char* err = nullptr;
	int32_t len = 0;
	urnet_generate_shared_secret(privateKey.data(), (int32_t)privateKey.size(), publicKey.data(), (int32_t)publicKey.size(), nullptr, &len, &err);
	if (err) {
		detail::throwError(err);
	}
	std::vector<uint8_t> out((size_t)len);
	if (0 < len) {
		if (!urnet_generate_shared_secret(privateKey.data(), (int32_t)privateKey.size(), publicKey.data(), (int32_t)publicKey.size(), out.data(), &len, &err)) {
			detail::throwError(err);
		}
	}
	out.resize((size_t)len);
	return out;
}

`

const hppPrelude = `/* Code generated by gen/gen.go. DO NOT EDIT.
 *
 * URnetwork SDK c++17 wrapper over the c abi (urnetwork_sdk.h).
 * Header-only. Requires nlohmann/json on the include path.
 *
 * - behavioral objects are raii handle wrappers: the handle is released on
 *   destruction. destruction does not close/stop the underlying object except
 *   Sub, which closes (unsubscribes) on destruction like a scoped connection.
 * - data types are structs that serialize with nlohmann-json.
 * - callbacks are std::function and may fire on arbitrary threads; marshal to
 *   your ui thread. do not destroy state captured by a listener while a
 *   callback may be in flight.
 * - functions that can fail throw urnet::Error.
 */
#ifndef URNETWORK_SDK_HPP
#define URNETWORK_SDK_HPP

#include <cstdint>
#include <cstdio>
#include <functional>
#include <map>
#include <memory>
#include <optional>
#include <stdexcept>
#include <string>
#include <utility>
#include <vector>

#include <nlohmann/json.hpp>

#include "urnetwork_sdk.h"

namespace urnet {

class Error : public std::runtime_error {
public:
	using std::runtime_error::runtime_error;
};

namespace detail {

/* take ownership of a returned char*, freeing it */
inline std::string takeString(char* s) {
	if (!s) {
		return {};
	}
	std::string r(s);
	urnet_free_string(s);
	return r;
}

inline std::optional<std::string> takeStringOpt(char* s) {
	if (!s) {
		return std::nullopt;
	}
	std::string r(s);
	urnet_free_string(s);
	return r;
}

[[noreturn]] inline void throwError(char* err) {
	std::string m = err ? takeString(err) : std::string("urnet: unknown error");
	throw Error(m);
}

template <typename T>
inline T parseJson(const char* s) {
	try {
		return nlohmann::json::parse(s).template get<T>();
	} catch (const std::exception& e) {
		throw Error(std::string("urnet: json: ") + e.what());
	}
}

/* move-only owner of a c handle */
class Handle {
public:
	Handle() = default;
	explicit Handle(uint64_t h) : h_(h) {}
	Handle(const Handle&) = delete;
	Handle& operator=(const Handle&) = delete;
	Handle(Handle&& o) noexcept : h_(o.h_), retained_(std::move(o.retained_)) { o.h_ = 0; }
	Handle& operator=(Handle&& o) noexcept {
		if (this != &o) {
			reset();
			h_ = o.h_;
			o.h_ = 0;
			retained_ = std::move(o.retained_);
		}
		return *this;
	}
	~Handle() { reset(); }

	uint64_t handle() const { return h_; }
	explicit operator bool() const { return h_ != 0; }

	/* release the c handle (does not close/stop the underlying object) */
	void reset() {
		if (h_) {
			urnet_release(h_);
			h_ = 0;
		}
		retained_.clear();
	}

	/* keep a callback alive as long as this wrapper */
	void retain(std::shared_ptr<void> p) const { retained_.push_back(std::move(p)); }

protected:
	uint64_t h_ = 0;
	mutable std::vector<std::shared_ptr<void>> retained_;
};

} // namespace detail
`

// writeHpp emits include/urnetwork_sdk.hpp from the captured export model
func (g *gen) writeHpp() error {
	var b strings.Builder
	b.WriteString(hppPrelude)
	b.WriteString("\n")

	// ----- constants
	if 0 < len(g.constants) {
		b.WriteString("/* ----- constants ----- */\n\n")
		sorted := append([]constRecord{}, g.constants...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].goName < sorted[j].goName })
		for _, c := range sorted {
			cppType := "int64_t"
			if strings.HasPrefix(c.literal, `"`) {
				cppType = "const char*"
			} else if strings.ContainsAny(c.literal, ".eE") {
				cppType = "double"
			}
			fmt.Fprintf(&b, "inline constexpr %s %s = %s;\n", cppType, c.goName, c.literal)
		}
		b.WriteString("\n")
	}

	// ----- forward declarations for behavioral classes
	classes := g.hppClasses()
	b.WriteString("/* ----- classes ----- */\n\n")
	for _, class := range classes {
		if !unixOnlySymbols[class] {
			fmt.Fprintf(&b, "class %s;\n", class)
		}
	}
	b.WriteString("#if !defined(_WIN32)\n")
	for _, class := range classes {
		if unixOnlySymbols[class] {
			fmt.Fprintf(&b, "class %s;\n", class)
		}
	}
	b.WriteString("#endif\n\n")

	// ----- data types
	b.WriteString("/* ----- data types (json) ----- */\n\n")
	if err := g.hppDataTypes(&b); err != nil {
		return err
	}

	// ----- callback aliases
	b.WriteString("/* ----- callbacks (fire on arbitrary threads) ----- */\n\n")
	names := make([]string, 0, len(callbackCache))
	for name := range callbackCache {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !callbackCache[name].unixOnly {
			g.hppCallbackAlias(&b, callbackCache[name])
		}
	}
	b.WriteString("#if !defined(_WIN32)\n")
	for _, name := range names {
		if callbackCache[name].unixOnly {
			g.hppCallbackAlias(&b, callbackCache[name])
		}
	}
	b.WriteString("#endif\n\n")

	// ----- class definitions
	classExports := map[string][]*export{}
	var funcExports []*export
	for _, e := range g.exports {
		if e.sig == nil {
			continue
		}
		if e.sig.recvName == "" {
			funcExports = append(funcExports, e)
		} else {
			classExports[e.sig.recvName] = append(classExports[e.sig.recvName], e)
		}
	}
	for _, class := range classes {
		g.hppClassDef(&b, class, classExports[class])
	}

	// ----- trampolines (after classes: they construct wrappers)
	b.WriteString("namespace detail {\n\n")
	for _, name := range names {
		info := callbackCache[name]
		if info.unixOnly {
			b.WriteString("#if !defined(_WIN32)\n")
		}
		g.hppTrampolines(&b, info)
		if info.unixOnly {
			b.WriteString("#endif\n")
		}
	}
	b.WriteString("} // namespace detail\n\n")

	// ----- method definitions
	b.WriteString("/* ----- method definitions ----- */\n\n")
	for _, class := range classes {
		exports := classExports[class]
		if len(exports) == 0 {
			continue
		}
		if unixOnlySymbols[class] {
			b.WriteString("#if !defined(_WIN32)\n")
		}
		for _, e := range exports {
			g.hppCallable(&b, e, class)
		}
		if unixOnlySymbols[class] {
			b.WriteString("#endif\n")
		}
	}

	// ----- free functions
	b.WriteString("/* ----- functions ----- */\n\n")
	b.WriteString("/* the sdk version this library was built from */\ninline std::string version() { return detail::takeString(urnet_version()); }\n")
	b.WriteString("/* number of live handles, for leak checks */\ninline int64_t liveHandleCount() { return urnet_live_handle_count(); }\n\n")
	for _, e := range funcExports {
		if e.unixOnly {
			b.WriteString("#if !defined(_WIN32)\n")
		}
		g.hppCallable(&b, e, "")
		if e.unixOnly {
			b.WriteString("#endif\n")
		}
	}
	b.WriteString("\n")

	b.WriteString(hppExtraDefinitions)
	b.WriteString("} // namespace urnet\n\n#endif /* URNETWORK_SDK_HPP */\n")

	return os.WriteFile("include/urnetwork_sdk.hpp", []byte(b.String()), 0644)
}

// hppClasses returns the behavioral class names in definition order
// (Device and Sub first: DeviceLocal/DeviceRemote derive from Device, and
// listener methods return Sub by value)
func (g *gen) hppClasses() []string {
	var classes []string
	for name := range behavioralTypes {
		classes = append(classes, name)
	}
	sort.Strings(classes)
	sort.SliceStable(classes, func(i, j int) bool {
		rank := func(n string) int {
			switch n {
			case "Sub":
				return 0
			case "Device":
				return 1
			}
			return 2
		}
		return rank(classes[i]) < rank(classes[j])
	})
	return classes
}

func (g *gen) hppClassDef(b *strings.Builder, class string, exports []*export) {
	unix := unixOnlySymbols[class]
	if unix {
		b.WriteString("#if !defined(_WIN32)\n")
	}
	base := "detail::Handle"
	if g.deviceDerived[class] {
		base = "Device"
	}
	final := " final"
	if class == "Device" {
		final = ""
	}
	fmt.Fprintf(b, "class %s%s : public %s {\npublic:\n", class, final, base)
	fmt.Fprintf(b, "\t%s() = default;\n", class)
	fmt.Fprintf(b, "\texplicit %s(uint64_t h) : %s(h) {}\n", class, base)
	if class == "Sub" {
		b.WriteString(`	/* closes (unsubscribes) on destruction, like a scoped connection */
	~Sub() {
		if (h_) {
			urnet_sub_close(h_);
		}
	}
	Sub(Sub&&) = default;
	Sub& operator=(Sub&& o) noexcept {
		if (this != &o && h_) {
			urnet_sub_close(h_);
		}
		detail::Handle::operator=(std::move(o));
		return *this;
	}
`)
	}
	for _, e := range exports {
		fmt.Fprintf(b, "\t%s;\n", g.hppSignature(e, class, false))
	}
	if extra, ok := hppExtraClassDecls[class]; ok {
		b.WriteString(extra)
	}
	b.WriteString("};\n")
	if unix {
		b.WriteString("#endif\n")
	}
	b.WriteString("\n")
}

// cppJsonType maps a json-crossing type to its c++ representation (without the
// optional wrapper for pointers, which the caller applies)
func (g *gen) cppJsonType(t types.Type) string {
	t = types.Unalias(t)
	switch t := t.(type) {
	case *types.Pointer:
		return g.cppJsonType(t.Elem())
	case *types.Slice:
		if basic, ok := types.Unalias(t.Elem()).(*types.Basic); ok && basic.Kind() == types.Uint8 {
			return "std::string" // base64 in json
		}
		return "std::vector<" + g.cppJsonType(t.Elem()) + ">"
	case *types.Map:
		return "std::map<std::string, " + g.cppJsonType(t.Elem()) + ">"
	case *types.Basic:
		return cppBasic(t)
	case *types.Named:
		name := t.Obj().Name()
		if t.Obj().Pkg() != nil && t.Obj().Pkg().Path() == sdkPath {
			switch name {
			case "Id", "Time":
				return "std::string"
			}
			if behavioralTypes[name] {
				// behavioral objects have no meaningful json shape
				return "nlohmann::json"
			}
			if _, ok := t.Underlying().(*types.Struct); ok {
				return name
			}
			return g.cppJsonType(t.Underlying())
		}
		switch t.String() {
		case "time.Time":
			return "std::string"
		case "time.Duration":
			return "int64_t"
		case "net/netip.Addr":
			return "std::string"
		}
		if basic, ok := t.Underlying().(*types.Basic); ok {
			return cppBasic(basic)
		}
		return "nlohmann::json"
	case *types.Interface:
		return "nlohmann::json"
	}
	return "nlohmann::json"
}

func cppBasic(t *types.Basic) string {
	switch t.Kind() {
	case types.Bool, types.UntypedBool:
		return "bool"
	case types.Int8, types.Int16, types.Int32:
		return "int32_t"
	case types.Int, types.Int64, types.UntypedInt:
		return "int64_t"
	case types.Uint8, types.Uint16, types.Uint32:
		return "uint32_t"
	case types.Uint, types.Uint64:
		return "uint64_t"
	case types.Float32:
		return "float"
	case types.Float64, types.UntypedFloat:
		return "double"
	case types.String, types.UntypedString:
		return "std::string"
	}
	return "nlohmann::json"
}

type hppField struct {
	cppName  string
	jsonName string
	cppType  string
	optional bool
}

// hppStructModel returns the fields of a data struct, or listElem set for
// list wrapper types (which become vector aliases)
func (g *gen) hppStructModel(named *types.Named) (fields []hppField, listElem string) {
	st := named.Underlying().(*types.Struct)
	for i := 0; i < st.NumFields(); i += 1 {
		f := st.Field(i)
		if f.Embedded() {
			if strings.Contains(f.Type().String(), "exportedList") {
				elem := "nlohmann::json"
				if n, ok := types.Unalias(f.Type()).(*types.Named); ok {
					if n.TypeArgs() != nil && n.TypeArgs().Len() == 1 {
						elem = g.cppJsonType(n.TypeArgs().At(0))
					}
				}
				return nil, elem
			}
			continue
		}
		if !f.Exported() {
			continue
		}
		tag := reflectTagLookup(st.Tag(i), "json")
		jsonName := f.Name()
		omitEmpty := false
		if tag != "" {
			parts := strings.Split(tag, ",")
			if parts[0] == "-" {
				continue
			}
			if parts[0] != "" {
				jsonName = parts[0]
			}
			for _, p := range parts[1:] {
				if p == "omitempty" {
					omitEmpty = true
				}
			}
		}
		_, isPointer := types.Unalias(f.Type()).(*types.Pointer)
		fields = append(fields, hppField{
			cppName:  cppName(jsonName),
			jsonName: jsonName,
			cppType:  g.cppJsonType(f.Type()),
			optional: isPointer || omitEmpty,
		})
	}
	return fields, ""
}

// hppDataTypes emits forward declarations, list aliases, struct definitions in
// dependency order, and to/from json functions
func (g *gen) hppDataTypes(b *strings.Builder) error {
	names := make([]string, 0, len(g.dataTypes))
	for name := range g.dataTypes {
		names = append(names, name)
	}
	sort.Strings(names)

	models := map[string][]hppField{}
	listElems := map[string]string{}
	for _, name := range names {
		fields, listElem := g.hppStructModel(g.dataTypes[name])
		if listElem != "" {
			listElems[name] = listElem
		} else {
			models[name] = fields
		}
	}

	// a struct must be defined after the types it holds directly or inside
	// std::optional / std::map (std::vector supports incomplete types)
	deps := map[string][]string{}
	for name, fields := range models {
		for _, f := range fields {
			for other := range models {
				if f.cppType == other || strings.Contains(f.cppType, "std::map<std::string, "+other+">") {
					deps[name] = append(deps[name], other)
				}
			}
		}
	}

	var order []string
	state := map[string]int{} // 0 unvisited, 1 visiting, 2 done
	var visit func(string) error
	visit = func(name string) error {
		switch state[name] {
		case 1:
			return fmt.Errorf("data type cycle involving %s", name)
		case 2:
			return nil
		}
		state[name] = 1
		sort.Strings(deps[name])
		for _, d := range deps[name] {
			if d != name {
				if err := visit(d); err != nil {
					return err
				}
			}
		}
		state[name] = 2
		order = append(order, name)
		return nil
	}
	for _, name := range names {
		if _, ok := models[name]; ok {
			if err := visit(name); err != nil {
				return err
			}
		}
	}

	// forward declarations, then list aliases, then definitions
	for _, name := range order {
		fmt.Fprintf(b, "struct %s;\n", name)
	}
	b.WriteString("\n")
	for _, name := range names {
		if elem, ok := listElems[name]; ok {
			fmt.Fprintf(b, "using %s = std::vector<%s>;\n", name, elem)
		}
	}
	b.WriteString("\n")

	for _, name := range order {
		fields := models[name]
		fmt.Fprintf(b, "struct %s {\n", name)
		for _, f := range fields {
			if f.optional {
				fmt.Fprintf(b, "\tstd::optional<%s> %s;\n", f.cppType, f.cppName)
			} else {
				fmt.Fprintf(b, "\t%s %s{};\n", f.cppType, f.cppName)
			}
		}
		b.WriteString("};\n\n")
	}

	// prototypes first so nested conversions resolve regardless of order
	for _, name := range order {
		fmt.Fprintf(b, "inline void to_json(nlohmann::json& j, const %s& v);\n", name)
		fmt.Fprintf(b, "inline void from_json(const nlohmann::json& j, %s& v);\n", name)
	}
	b.WriteString("\n")

	for _, name := range order {
		fields := models[name]
		fmt.Fprintf(b, "inline void to_json(nlohmann::json& j, const %s& v) {\n\tj = nlohmann::json::object();\n", name)
		for _, f := range fields {
			if f.optional {
				fmt.Fprintf(b, "\tif (v.%s) {\n\t\tj[\"%s\"] = *v.%s;\n\t}\n", f.cppName, f.jsonName, f.cppName)
			} else {
				fmt.Fprintf(b, "\tj[\"%s\"] = v.%s;\n", f.jsonName, f.cppName)
			}
		}
		b.WriteString("}\n")
		fmt.Fprintf(b, "inline void from_json(const nlohmann::json& j, %s& v) {\n\tif (!j.is_object()) {\n\t\treturn;\n\t}\n", name)
		for _, f := range fields {
			if f.optional {
				fmt.Fprintf(b, "\tif (auto it = j.find(\"%s\"); it != j.end() && !it->is_null()) {\n\t\t%s tmp{};\n\t\tit->get_to(tmp);\n\t\tv.%s = std::move(tmp);\n\t}\n", f.jsonName, f.cppType, f.cppName)
			} else {
				fmt.Fprintf(b, "\tif (auto it = j.find(\"%s\"); it != j.end() && !it->is_null()) {\n\t\tit->get_to(v.%s);\n\t}\n", f.jsonName, f.cppName)
			}
		}
		b.WriteString("}\n\n")
	}
	return nil
}

func (g *gen) cppCallbackParams(cm *callbackMethod) []string {
	var params []string
	for _, p := range cm.params {
		n := cppName(snake(p.name))
		switch p.info.kind {
		case kindBool:
			params = append(params, "bool "+n)
		case kindInt, kindTime:
			params = append(params, "int64_t "+n)
		case kindFloat64:
			params = append(params, "double "+n)
		case kindFloat32:
			params = append(params, "float "+n)
		case kindString, kindId:
			params = append(params, "std::string "+n)
		case kindHandle:
			params = append(params, p.info.named.Obj().Name()+" "+n)
		case kindJson:
			if p.info.pointer {
				params = append(params, "std::optional<"+g.cppJsonType(p.info.t)+"> "+n)
			} else {
				params = append(params, g.cppJsonType(p.info.t)+" "+n)
			}
		case kindBytes:
			params = append(params, "const uint8_t* "+n, "int32_t "+n+"_len")
		case kindError:
			params = append(params, "std::optional<std::string> "+n)
		}
	}
	return params
}

func (g *gen) cppCallbackRet(cm *callbackMethod) string {
	if cm.result == nil {
		return "void"
	}
	switch cm.result.kind {
	case kindBool:
		return "bool"
	case kindInt:
		return "int64_t"
	case kindString:
		return "std::string"
	}
	return "void"
}

func (g *gen) hppCallbackAlias(b *strings.Builder, info *callbackInfo) {
	if len(info.methods) == 1 {
		cm := info.methods[0]
		fmt.Fprintf(b, "using %s = std::function<%s(%s)>;\n", info.ifaceName, g.cppCallbackRet(cm), strings.Join(g.cppCallbackParams(cm), ", "))
		return
	}
	fmt.Fprintf(b, "struct %s {\n", info.ifaceName)
	for _, cm := range info.methods {
		fmt.Fprintf(b, "\tstd::function<%s(%s)> %s;\n", g.cppCallbackRet(cm), strings.Join(g.cppCallbackParams(cm), ", "), cppName(snake(cm.name)))
	}
	b.WriteString("};\n")
}

func trampolineName(cm *callbackMethod) string {
	return strings.TrimSuffix(strings.TrimPrefix(cm.typedefName, "urnet_"), "_cb")
}

// hppTrampolines emits the c-compatible trampoline functions for a callback
func (g *gen) hppTrampolines(b *strings.Builder, info *callbackInfo) {
	for _, cm := range info.methods {
		cSig := "void* user_data"
		if 0 < len(cm.cParams) {
			cSig += ", " + strings.Join(cm.cParams, ", ")
		}
		ret := g.cppCallbackRet(cm)

		// conversions from the c params to the c++ invocation args
		var conv []string
		var args []string
		for _, p := range cm.params {
			n := cppName(snake(p.name))
			switch p.info.kind {
			case kindBool, kindInt, kindTime, kindFloat64, kindFloat32:
				args = append(args, n)
			case kindString, kindId:
				args = append(args, fmt.Sprintf("std::string(%s ? %s : \"\")", n, n))
			case kindHandle:
				conv = append(conv, fmt.Sprintf("\t\t%s %s_v(%s);", p.info.named.Obj().Name(), n, n))
				args = append(args, fmt.Sprintf("std::move(%s_v)", n))
			case kindJson:
				t := g.cppJsonType(p.info.t)
				// the c parameter carries the _json suffix (see gen.go)
				jn := n + "_json"
				if p.info.pointer {
					conv = append(conv,
						fmt.Sprintf("\t\tstd::optional<%s> %s_v;", t, n),
						fmt.Sprintf("\t\tif (%s) {", jn),
						fmt.Sprintf("\t\t\t%s_v = parseJson<%s>(%s);", n, t, jn),
						"\t\t}")
				} else {
					conv = append(conv,
						fmt.Sprintf("\t\t%s %s_v{};", t, n),
						fmt.Sprintf("\t\tif (%s) {", jn),
						fmt.Sprintf("\t\t\t%s_v = parseJson<%s>(%s);", n, t, jn),
						"\t\t}")
				}
				args = append(args, fmt.Sprintf("std::move(%s_v)", n))
			case kindBytes:
				args = append(args, n, n+"_len")
			case kindError:
				conv = append(conv,
					fmt.Sprintf("\t\tstd::optional<std::string> %s_v;", n),
					fmt.Sprintf("\t\tif (%s) {", n),
					fmt.Sprintf("\t\t\t%s_v = std::string(%s);", n, n),
					"\t\t}")
				args = append(args, fmt.Sprintf("std::move(%s_v)", n))
			}
		}

		invokeName := trampolineName(cm)
		argList := strings.Join(args, ", ")

		emitTramp := func(variant string, oneshot bool) {
			fmt.Fprintf(b, "inline %s %s_%s(%s) {\n", ret, variant, invokeName, cSig)
			fmt.Fprintf(b, "\tauto* f = static_cast<%s*>(user_data);\n", info.ifaceName)
			if ret != "void" {
				fmt.Fprintf(b, "\t%s r{};\n", ret)
			}
			b.WriteString("\ttry {\n")
			for _, c := range conv {
				b.WriteString(c)
				b.WriteString("\n")
			}
			assign := ""
			if ret != "void" {
				assign = "r = "
			}
			if len(info.methods) == 1 {
				fmt.Fprintf(b, "\t\t%s(*f)(%s);\n", assign, argList)
			} else {
				field := cppName(snake(cm.name))
				fmt.Fprintf(b, "\t\tif (f->%s) {\n\t\t\t%sf->%s(%s);\n\t\t}\n", field, assign, field, argList)
			}
			b.WriteString("\t} catch (const std::exception& e) {\n\t\tstd::fprintf(stderr, \"urnet callback error: %s\\n\", e.what());\n\t} catch (...) {\n\t}\n")
			if oneshot {
				b.WriteString("\tdelete f;\n")
			}
			if ret != "void" {
				b.WriteString("\treturn r;\n")
			}
			b.WriteString("}\n")
		}

		emitTramp("retained", false)
		if len(info.methods) == 1 {
			emitTramp("oneshot", true)
		}
	}
	b.WriteString("\n")
}

// callbackPolicy decides the lifetime of a std::function passed at a call site:
//   - "sub": owned by the returned Sub
//   - "object": owned by the returned wrapper object
//   - "oneshot": self-deleting after the single result/complete invocation
//   - "receiver": owned by the receiver wrapper
func (g *gen) callbackPolicy(sig *exportSig, cb *callbackInfo) string {
	if sig.result != nil && sig.result.kind == kindHandle {
		if sig.result.named != nil && sig.result.named.Obj().Name() == "Sub" {
			return "sub"
		}
		return "object"
	}
	if len(cb.methods) == 1 {
		switch cb.methods[0].name {
		case "Result", "Complete":
			return "oneshot"
		}
	}
	return "receiver"
}

// hppSignature renders a method/function signature (decl or definition head)
func (g *gen) hppSignature(e *export, class string, definition bool) string {
	sig := e.sig
	var params []string
	for _, p := range sig.params {
		n := cppName(snake(p.name))
		switch p.info.kind {
		case kindBool:
			params = append(params, "bool "+n)
		case kindInt, kindTime:
			params = append(params, "int64_t "+n)
		case kindFloat64:
			params = append(params, "double "+n)
		case kindFloat32:
			params = append(params, "float "+n)
		case kindString, kindId:
			params = append(params, "const std::string& "+n)
		case kindHandle:
			params = append(params, "const "+p.info.named.Obj().Name()+"& "+n)
		case kindJson:
			if p.info.pointer {
				params = append(params, "const std::optional<"+g.cppJsonType(p.info.t)+">& "+n)
			} else {
				params = append(params, "const "+g.cppJsonType(p.info.t)+"& "+n)
			}
		case kindBytes:
			params = append(params, "const uint8_t* "+n, "int32_t "+n+"_len")
		case kindCallback:
			params = append(params, p.cb.ifaceName+" "+n)
		}
	}

	ret := "void"
	if sig.result != nil {
		switch sig.result.kind {
		case kindBool:
			ret = "bool"
		case kindInt, kindTime:
			ret = "int64_t"
		case kindFloat64:
			ret = "double"
		case kindFloat32:
			ret = "float"
		case kindString, kindId:
			ret = "std::string"
		case kindHandle:
			ret = sig.result.named.Obj().Name()
		case kindJson:
			if sig.result.pointer {
				ret = "std::optional<" + g.cppJsonType(sig.result.t) + ">"
			} else {
				ret = g.cppJsonType(sig.result.t)
			}
		}
	}

	name := lowerCamel(sig.goName)
	if definition && class != "" {
		name = class + "::" + name
	}
	qual := ""
	if class != "" {
		qual = " const"
	}
	prefix := ""
	if definition {
		prefix = "inline "
	}
	return fmt.Sprintf("%s%s %s(%s)%s", prefix, ret, name, strings.Join(params, ", "), qual)
}

// hppCallable renders the inline definition of a method or free function
func (g *gen) hppCallable(b *strings.Builder, e *export, class string) {
	sig := e.sig
	var body []string
	var callArgs []string
	if class != "" {
		callArgs = append(callArgs, "handle()")
	}

	var retainLines []string // emitted after a handle result is constructed

	for _, p := range sig.params {
		n := cppName(snake(p.name))
		switch p.info.kind {
		case kindBool, kindInt, kindTime, kindFloat64, kindFloat32:
			callArgs = append(callArgs, n)
		case kindString, kindId:
			callArgs = append(callArgs, n+".c_str()")
		case kindHandle:
			callArgs = append(callArgs, n+".handle()")
		case kindJson:
			if p.info.pointer {
				body = append(body,
					fmt.Sprintf("\tstd::string %s_json;", n),
					fmt.Sprintf("\tconst char* %s_c = nullptr;", n),
					fmt.Sprintf("\tif (%s) {", n),
					fmt.Sprintf("\t\t%s_json = nlohmann::json(*%s).dump();", n, n),
					fmt.Sprintf("\t\t%s_c = %s_json.c_str();", n, n),
					"\t}")
				callArgs = append(callArgs, n+"_c")
			} else {
				body = append(body, fmt.Sprintf("\tstd::string %s_json = nlohmann::json(%s).dump();", n, n))
				callArgs = append(callArgs, n+"_json.c_str()")
			}
		case kindBytes:
			callArgs = append(callArgs, n, n+"_len")
		case kindCallback:
			policy := g.callbackPolicy(sig, p.cb)
			target := p.cb.ifaceName
			if policy == "oneshot" {
				body = append(body, fmt.Sprintf("\tauto* %s_fn = %s ? new %s(std::move(%s)) : nullptr;", n, cbTruthy(p.cb, n), target, n))
			} else {
				body = append(body,
					fmt.Sprintf("\tstd::shared_ptr<%s> %s_fn;", target, n),
					fmt.Sprintf("\tif (%s) {", cbTruthy(p.cb, n)),
					fmt.Sprintf("\t\t%s_fn = std::make_shared<%s>(std::move(%s));", n, target, n),
					"\t}")
			}
			variant := "retained"
			if policy == "oneshot" {
				variant = "oneshot"
			}
			for _, cm := range p.cb.methods {
				callArgs = append(callArgs, fmt.Sprintf("%s_fn ? &detail::%s_%s : nullptr", n, variant, trampolineName(cm)))
			}
			if policy == "oneshot" {
				callArgs = append(callArgs, n+"_fn")
			} else {
				callArgs = append(callArgs, n+"_fn.get()")
				switch policy {
				case "sub", "object":
					retainLines = append(retainLines,
						fmt.Sprintf("\tif (%s_fn) {", n),
						fmt.Sprintf("\t\tr.retain(%s_fn);", n),
						"\t}")
				case "receiver":
					body = append(body,
						fmt.Sprintf("\tif (%s_fn) {", n),
						fmt.Sprintf("\t\tretain(%s_fn);", n),
						"\t}")
				}
			}
		}
	}

	errCall := ""
	if sig.hasError {
		body = append(body, "\tchar* err_c = nullptr;")
		callArgs = append(callArgs, "&err_c")
		errCall = "\tif (err_c) {\n\t\tdetail::throwError(err_c);\n\t}"
	}

	call := fmt.Sprintf("%s(%s)", e.cName, strings.Join(callArgs, ", "))

	fmt.Fprintf(b, "%s {\n", g.hppSignature(e, class, true))

	switch {
	case sig.result == nil && !sig.hasError:
		body = append(body, "\t"+call+";")
	case sig.result == nil && sig.hasError:
		body = append(body,
			"\tbool ok = "+call+";",
			errCall,
			fmt.Sprintf("\tif (!ok) {\n\t\tthrow Error(\"urnet: %s failed\");\n\t}", e.cName))
	default:
		switch sig.result.kind {
		case kindBool:
			body = append(body, "\tbool r = "+call+";")
		case kindInt, kindTime:
			body = append(body, "\tint64_t r = "+call+";")
		case kindFloat64:
			body = append(body, "\tdouble r = "+call+";")
		case kindFloat32:
			body = append(body, "\tfloat r = "+call+";")
		case kindString, kindId, kindJson:
			body = append(body, "\tchar* r_c = "+call+";")
		case kindHandle:
			body = append(body, fmt.Sprintf("\t%s r(%s);", sig.result.named.Obj().Name(), call))
		}
		if sig.hasError {
			body = append(body, errCall)
		}
		body = append(body, retainLines...)
		switch sig.result.kind {
		case kindBool, kindInt, kindTime, kindFloat64, kindFloat32, kindHandle:
			body = append(body, "\treturn r;")
		case kindString, kindId:
			body = append(body, "\treturn detail::takeString(r_c);")
		case kindJson:
			t := g.cppJsonType(sig.result.t)
			if sig.result.pointer {
				body = append(body,
					"\tauto r_s = detail::takeStringOpt(r_c);",
					"\tif (!r_s) {\n\t\treturn std::nullopt;\n\t}",
					fmt.Sprintf("\treturn detail::parseJson<%s>(r_s->c_str());", t))
			} else {
				body = append(body,
					"\tauto r_s = detail::takeStringOpt(r_c);",
					fmt.Sprintf("\tif (!r_s) {\n\t\treturn %s{};\n\t}", t),
					fmt.Sprintf("\treturn detail::parseJson<%s>(r_s->c_str());", t))
			}
		}
	}

	for _, line := range body {
		if line != "" {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	b.WriteString("}\n")
}

// cbTruthy renders the "callback provided" check for single or multi-method callbacks
func cbTruthy(cb *callbackInfo, n string) string {
	if len(cb.methods) == 1 {
		return n
	}
	var checks []string
	for _, cm := range cb.methods {
		checks = append(checks, "static_cast<bool>("+n+"."+cppName(snake(cm.name))+")")
	}
	return "(" + strings.Join(checks, " || ") + ")"
}
