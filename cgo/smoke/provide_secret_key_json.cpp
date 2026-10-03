// Standalone executable controls for the freshly generated value serializer.
// No SDK handles, Go runtime, files, network, or device constructors are used.
#include "urnetwork_sdk.hpp"

#include <initializer_list>
#include <iostream>
#include <type_traits>

namespace {

using Json = nlohmann::json;
using Key = urnet::ProvideSecretKey;

static_assert(std::is_same<decltype(Key{}.provide_secret_key), std::string>::value,
    "the public secret field must remain a raw-byte std::string");
static_assert(std::is_same<urnet::ProvideSecretKeyList, std::vector<Key>>::value,
    "the public secret list must remain a value vector");

// Fail with a fixed assertion class; neither decoded values nor exceptions
// from an unexpected JSON implementation are printed by this executable.
class AssertionFailure : public std::runtime_error {
public:
    explicit AssertionFailure(const char* failure) : std::runtime_error(failure) {}
};

void require(bool condition, const char* failure) {
    if (!condition) { throw AssertionFailure(failure); }
}

std::string bytes(std::initializer_list<unsigned int> values) {
    std::string result;
    for (auto value : values) {
        require(value <= 255, "fixture-byte-range");
        result.push_back(static_cast<char>(value));
    }
    return result;
}

std::string everyByte() {
    std::string result;
    for (unsigned int value = 0; value <= 255; value += 1) {
        result.push_back(static_cast<char>(value));
    }
    return result;
}

Key keyWith(const std::string& raw, int64_t mode = 37) {
    Key result;
    result.provide_mode = mode;
    result.provide_secret_key = raw;
    return result;
}

// This is the actual ADL to_json -> JSON text -> ADL from_json path. Expected
// representation is a fixture fact, not computed by a second UTF-8 codec.
void roundTrip(const std::string& raw, bool binary) {
    const Key original = keyWith(raw, -37);
    const Json encoded = original;
    const std::string serialized = encoded.dump();
    require(encoded.is_object() && encoded.size() == 2, "serialized-field-count");
    require(encoded.at("provide_mode").get<int64_t>() == -37, "serialized-mode");
    require(encoded.contains("provide_secret_key_base64") == binary, "serialized-binary-branch");
    require(encoded.contains("provide_secret_key") != binary, "serialized-plain-branch");
    if (!binary) {
        require(encoded.at("provide_secret_key").get<std::string>() == raw, "serialized-plain-bytes");
    }
    const Key decoded = Json::parse(serialized).get<Key>();
    require(decoded.provide_mode == original.provide_mode, "round-trip-mode");
    require(decoded.provide_secret_key == raw, "round-trip-raw-bytes");
    require(original.provide_secret_key == raw, "serialize-mutated-source");
}

// A failed object conversion must not partially adopt mode or plain bytes.
void rejectDocument(const std::string& document) {
    const Json input = Json::parse(document);
    Key receiver = keyWith(bytes({0x00, 0xff, 0x41}), 81);
    const Key before = receiver;
    bool rejected = false;
    try {
        input.get_to(receiver);
    } catch (const urnet::Error& error) {
        rejected = true;
        require(std::string(error.what()) == "decode provide secret key", "decode-error-not-fixed");
    }
    require(rejected, "malformed-record-was-accepted");
    require(receiver.provide_mode == before.provide_mode, "failed-decode-mutated-mode");
    require(receiver.provide_secret_key == before.provide_secret_key, "failed-decode-mutated-bytes");
}

void binary() {
    roundTrip(everyByte(), true);
    roundTrip(bytes({0xff}), true);
    roundTrip(bytes({0x00, 0x01, 0xfe, 0xff}), true);
    const Json single = keyWith(bytes({0xff}));
    require(single.at("provide_secret_key_base64").get<std::string>() == "/w==", "canonical-one-byte");
    const Json mixed = keyWith(bytes({0x00, 0x01, 0xfe, 0xff}));
    require(mixed.at("provide_secret_key_base64").get<std::string>() == "AAH+/w==", "canonical-mixed-bytes");
    for (unsigned int value = 0; value <= 255; value += 1) {
        roundTrip(bytes({value}), 128 <= value);
    }
}

void legacy() {
    for (const auto& raw : std::vector<std::string>{
        "", "plain", "base64:/w==", "base64:AAH+/w==", "b64:literal", "/w==",
        "provide_secret_key_base64:literal", std::string("a\0b", 3),
        bytes({0xc3, 0xa9, 0xe2, 0x98, 0x83, 0xf0, 0x9f, 0x8c, 0x8d}),
    }) {
        roundTrip(raw, false);
    }
    for (const auto& document : std::vector<std::string>{
        "null", "{}", R"({"provide_mode":null})", R"({"provide_secret_key":null})",
        R"({"provide_mode":null,"provide_secret_key":null,"unknown":"ignored"})",
    }) {
        Key receiver = keyWith("base64:still-literal", 81);
        Json::parse(document).get_to(receiver);
        require(receiver.provide_mode == 81 && receiver.provide_secret_key == "base64:still-literal",
            "legacy-missing-or-null-changed-receiver");
    }
    const Key empty = Json::parse(R"({"provide_secret_key":""})").get<Key>();
    require(empty.provide_mode == 0 && empty.provide_secret_key.empty(), "legacy-empty-zero-value");
    const Key prefix = Json::parse(R"({"provide_mode":-9,"provide_secret_key":"base64:/w=="})").get<Key>();
    require(prefix.provide_mode == -9 && prefix.provide_secret_key == "base64:/w==", "legacy-prefix-decoded");
    for (const auto& document : std::vector<std::string>{
        R"({"provide_mode":9223372036854775807})", R"({"provide_mode":-9223372036854775808})",
    }) {
        const Json parsed = Json::parse(document);
        const Key decoded = parsed.get<Key>();
        require(decoded.provide_mode == parsed.at("provide_mode").get<int64_t>(), "legacy-integer-range");
    }
}

void decode() {
    struct Vector {
        const char* encoded;
        std::string raw;
    };
    const std::vector<Vector> vectors = {
        {"", ""}, {"AA==", bytes({0x00})}, {"/w==", bytes({0xff})},
        {"AAH+/w==", bytes({0x00, 0x01, 0xfe, 0xff})},
        {"TQ==", "M"}, {"TWE=", "Ma"}, {"TWFu", "Man"}, {"////", bytes({0xff, 0xff, 0xff})},
    };
    for (const auto& vector : vectors) {
        Json input = {{"provide_secret_key_base64", vector.encoded}, {"unknown", true}};
        Key receiver = keyWith("previous", 81);
        Json::parse(input.dump()).get_to(receiver);
        require(receiver.provide_mode == 81 && receiver.provide_secret_key == vector.raw,
            "binary-decode-known-vector");
        input["provide_secret_key"] = "";
        require(Json::parse(input.dump()).get<Key>().provide_secret_key == vector.raw,
            "binary-with-empty-plain");
        input["provide_secret_key"] = nullptr;
        input["provide_mode"] = nullptr;
        Json::parse(input.dump()).get_to(receiver);
        require(receiver.provide_mode == 81 && receiver.provide_secret_key == vector.raw,
            "binary-with-null-plain-and-mode");
    }
    const Key empty = Json::parse(R"({"provide_secret_key_base64":""})").get<Key>();
    const Json reencoded = empty;
    require(empty.provide_secret_key.empty() && reencoded.contains("provide_secret_key") &&
        !reencoded.contains("provide_secret_key_base64"), "binary-empty-canonicalizes-to-legacy");
}

void reject() {
    for (const auto& encoded : std::vector<std::string>{
        "A", "AA", "AAA", "AAAAA", "A===", "=AAA", "A=AA", "AA=A", "====",
        "AA==AAAA", "AAA=AAAA", "AA===", "AB==", "AAB=", "/x==", "//9=",
        "AA-_", "AA?=", "AA\n=", "AA\r=", "AA =", "AA\t=", " AA==", "AA== ",
        "AA==\r\n", "AA==\n\r\t ", "AAAA\n\n\n\n", std::string("AA\0=", 4),
    }) {
        const Json input = {{"provide_mode", 7}, {"provide_secret_key_base64", encoded}};
        rejectDocument(input.dump());
    }
    for (const auto& document : std::vector<std::string>{
        "true", "1", "\"text\"", "[]",
        R"({"provide_mode":"private-marker"})", R"({"provide_mode":true})",
        R"({"provide_mode":1.5})", R"({"provide_mode":1.0})", R"({"provide_mode":1e0})",
        R"({"provide_mode":9223372036854775808})", R"({"provide_mode":18446744073709551615})",
        R"({"provide_mode":[]})", R"({"provide_mode":{}})",
        R"({"provide_secret_key":false})", R"({"provide_secret_key":17})",
        R"({"provide_secret_key":[]})", R"({"provide_secret_key":{}})",
        R"({"provide_secret_key_base64":null})", R"({"provide_secret_key_base64":false})",
        R"({"provide_secret_key_base64":42})", R"({"provide_secret_key_base64":[]})",
        R"({"provide_secret_key_base64":{}})",
        R"({"provide_secret_key":"private-marker","provide_secret_key_base64":"AA=="})",
        R"({"provide_secret_key":"M","provide_secret_key_base64":"TQ=="})",
        R"({"provide_secret_key":"nonempty","provide_secret_key_base64":""})",
        R"({"provide_mode":7,"provide_secret_key":"private-marker","provide_secret_key_base64":"bad!"})",
        R"({"provide_mode":7,"provide_secret_key":17,"provide_secret_key_base64":"AA=="})",
    }) {
        rejectDocument(document);
    }
}

void utf8() {
    for (const auto& raw : std::vector<std::string>{
        bytes({0x00, 0x7f}), bytes({0xc2, 0x80}), bytes({0xdf, 0xbf}),
        bytes({0xe0, 0xa0, 0x80}), bytes({0xed, 0x9f, 0xbf}), bytes({0xee, 0x80, 0x80}),
        bytes({0xef, 0xbf, 0xbf}), bytes({0xf0, 0x90, 0x80, 0x80}), bytes({0xf4, 0x8f, 0xbf, 0xbf}),
    }) {
        roundTrip(raw, false);
    }
    for (const auto& raw : std::vector<std::string>{
        bytes({0x80}), bytes({0xbf}), bytes({0xc0, 0xaf}), bytes({0xc1, 0xbf}),
        bytes({0xc2}), bytes({0xc2, 0x7f}), bytes({0xdf, 0xc0}),
        bytes({0xe0, 0x80, 0x80}), bytes({0xed, 0xa0, 0x80}), bytes({0xed, 0xbf, 0xbf}),
        bytes({0xe1, 0x80}), bytes({0xe1, 0x80, 0x7f}),
        bytes({0xf0, 0x80, 0x80, 0x80}), bytes({0xf4, 0x90, 0x80, 0x80}),
        bytes({0xf5, 0x80, 0x80, 0x80}), bytes({0xf1, 0x80, 0x80}),
        bytes({0xf1, 0x80, 0x80, 0xc0}), bytes({0xff, 0xff}),
    }) {
        roundTrip(raw, true);
    }
}

void list() {
    const urnet::ProvideSecretKeyList original = {
        keyWith("", 0), keyWith("base64:/w==", 17), keyWith(std::string("a\0b", 3), -4),
        keyWith(everyByte(), 81), keyWith(bytes({0xc3, 0xa9}), 2),
    };
    const Json json = original;
    const std::string serialized = json.dump();
    const auto decoded = urnet::detail::parseJson<urnet::ProvideSecretKeyList>(serialized.c_str());
    require(decoded.size() == original.size(), "list-count");
    for (std::size_t index = 0; index < original.size(); index += 1) {
        require(decoded[index].provide_mode == original[index].provide_mode, "list-mode");
        require(decoded[index].provide_secret_key == original[index].provide_secret_key, "list-raw-bytes");
    }
    require(json.at(3).contains("provide_secret_key_base64") &&
        !json.at(3).contains("provide_secret_key"), "list-binary-element");
    require(urnet::detail::parseJson<urnet::ProvideSecretKeyList>("null").empty(), "legacy-null-list");
    bool rejected = false;
    try {
        urnet::detail::parseJson<urnet::ProvideSecretKeyList>(
            R"([{"provide_secret_key_base64":"private-marker"}])");
    } catch (const urnet::Error& error) {
        rejected = true;
        require(std::string(error.what()) == "urnet: json: decode provide secret key", "list-error-not-fixed");
    }
    require(rejected, "list-malformed-binary-accepted");
}

} // namespace

int main(int argc, char** argv) {
    struct Test {
        const char* name;
        void (*run)();
    };
    const Test tests[] = {
        {"binary", binary}, {"legacy", legacy}, {"decode", decode},
        {"reject", reject}, {"utf8", utf8}, {"list", list},
    };
    if (argc != 2) {
        std::cerr << "usage: provide_secret_key_json all|binary|legacy|decode|reject|utf8|list\n";
        return 64;
    }
    const std::string selected = argv[1];
    std::size_t executed = 0;
    for (const auto& test : tests) {
        if (selected != "all" && selected != test.name) { continue; }
        std::cout << "RUN " << test.name << std::endl;
        try {
            test.run();
        } catch (const AssertionFailure& error) {
            std::cerr << "FAIL " << test.name << " " << error.what() << "\n";
            return 1;
        } catch (const nlohmann::json::exception& error) {
            std::cerr << "FAIL " << test.name << " json-error-id=" << error.id << "\n";
            return 1;
        } catch (const std::exception&) {
            std::cerr << "FAIL " << test.name << " unexpected-exception\n";
            return 1;
        }
        std::cout << "PASS " << test.name << std::endl;
        executed += 1;
    }
    if (executed == 0) { return 64; }
    std::cout << "PASS total=" << executed << std::endl;
    return 0;
}
