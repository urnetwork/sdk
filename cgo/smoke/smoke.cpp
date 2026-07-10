// abi smoke test: exercises strings, ids, buffer-out, json data, handles,
// and the async callback machinery end to end against the built library.
// build and run with `make smoke` from the cgo module root.

#include <atomic>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <thread>
#include <unistd.h>

#include "urnetwork_sdk.h"

#define CHECK(cond)                                                              \
	do {                                                                         \
		if (!(cond)) {                                                           \
			std::fprintf(stderr, "FAIL %s:%d: %s\n", __FILE__, __LINE__, #cond); \
			return 1;                                                            \
		}                                                                        \
	} while (0)

static void onGetByJwt(void* userData, const char* result, bool ok) {
	(void)result;
	(void)ok;
	auto* count = static_cast<std::atomic<int>*>(userData);
	count->fetch_add(1);
}

int main() {
	// version and string ownership
	char* version = urnet_version();
	CHECK(version != nullptr);
	std::printf("version: \"%s\"\n", version);
	urnet_free_string(version);

	CHECK(urnet_live_handle_count() == 0);

	// id roundtrip
	char* id = urnet_new_id();
	CHECK(id != nullptr && std::strlen(id) == 36);
	char* error = nullptr;
	char* parsed = urnet_parse_id(id, &error);
	CHECK(parsed != nullptr && error == nullptr);
	CHECK(std::strcmp(parsed, id) == 0);
	urnet_free_string(parsed);
	urnet_free_string(id);

	char* bad = urnet_parse_id("not-an-id", &error);
	CHECK(bad == nullptr && error != nullptr);
	std::printf("expected parse error: %s\n", error);
	urnet_free_string(error);
	error = nullptr;

	// base58 roundtrip through the buffer-out pattern
	const uint8_t data[] = {1, 2, 3, 4, 5, 255, 0, 42};
	char* encoded = urnet_encode_base58(data, (int32_t)sizeof(data));
	CHECK(encoded != nullptr);
	uint8_t decoded[64];
	int32_t decodedLen = (int32_t)sizeof(decoded);
	CHECK(urnet_decode_base58(encoded, decoded, &decodedLen));
	CHECK(decodedLen == (int32_t)sizeof(data));
	CHECK(std::memcmp(decoded, data, sizeof(data)) == 0);
	// a short buffer reports the needed size and does not copy
	uint8_t one[1];
	int32_t shortLen = (int32_t)sizeof(one);
	CHECK(!urnet_decode_base58(encoded, one, &shortLen));
	CHECK(shortLen == (int32_t)sizeof(data));
	urnet_free_string(encoded);

	// data types cross as json
	char* proxyConfig = urnet_default_proxy_config();
	CHECK(proxyConfig != nullptr && proxyConfig[0] == '{');
	std::printf("default proxy config: %s\n", proxyConfig);
	urnet_free_string(proxyConfig);

	// handles and the async callback machinery
	char storageDir[] = "/tmp/urnet_smoke_XXXXXX";
	CHECK(mkdtemp(storageDir) != nullptr);

	uint64_t asyncLocalState = urnet_new_async_local_state(storageDir);
	CHECK(asyncLocalState != 0);
	uint64_t localState = urnet_async_local_state_get_local_state(asyncLocalState);
	CHECK(localState != 0);
	char* jwt = urnet_local_state_get_by_jwt(localState);
	CHECK(jwt != nullptr);
	urnet_free_string(jwt);

	std::atomic<int> callbackCount{0};
	urnet_async_local_state_get_by_jwt(asyncLocalState, onGetByJwt, &callbackCount);
	for (int i = 0; i < 500 && callbackCount.load() == 0; i += 1) {
		std::this_thread::sleep_for(std::chrono::milliseconds(10));
	}
	CHECK(callbackCount.load() == 1);

	urnet_async_local_state_close(asyncLocalState);
	CHECK(urnet_release(localState));
	CHECK(urnet_release(asyncLocalState));
	// a double release reports false
	CHECK(!urnet_release(asyncLocalState));
	CHECK(urnet_live_handle_count() == 0);

	std::printf("OK\n");
	return 0;
}
