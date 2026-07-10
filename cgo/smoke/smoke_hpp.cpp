// c++ wrapper smoke test: raii handles, typed json structs, exceptions,
// std::function callbacks, and handle leak balance through urnetwork_sdk.hpp.
// build and run with `make smoke_hpp` from the cgo module root.

#include <atomic>
#include <chrono>
#include <cstdio>
#include <thread>
#include <unistd.h>

#include "urnetwork_sdk.hpp"

#define CHECK(cond)                                                              \
	do {                                                                         \
		if (!(cond)) {                                                           \
			std::fprintf(stderr, "FAIL %s:%d: %s\n", __FILE__, __LINE__, #cond); \
			return 1;                                                            \
		}                                                                        \
	} while (0)

int main() {
	std::printf("version: \"%s\"\n", urnet::version().c_str());
	CHECK(urnet::liveHandleCount() == 0);

	// id roundtrip and the throwing error path
	std::string id = urnet::newId();
	CHECK(id.size() == 36);
	CHECK(urnet::parseId(id) == id);
	bool threw = false;
	try {
		urnet::parseId("not-an-id");
	} catch (const urnet::Error& e) {
		threw = true;
		std::printf("expected parse error: %s\n", e.what());
	}
	CHECK(threw);

	// base58 through the buffer-out wrapper
	std::vector<uint8_t> data{1, 2, 3, 4, 5, 255, 0, 42};
	std::string encoded = urnet::encodeBase58(data.data(), (int32_t)data.size());
	CHECK(!encoded.empty());
	auto decoded = urnet::decodeBase58(encoded);
	CHECK(decoded && *decoded == data);
	CHECK(!urnet::decodeBase58("!!! not base58 !!!"));

	// typed json data structs
	auto proxyConfig = urnet::defaultProxyConfig();
	CHECK(proxyConfig);
	CHECK(proxyConfig->enable_http == true);
	proxyConfig->enable_socks = true;
	nlohmann::json j = *proxyConfig;
	CHECK(j["enable_socks"] == true);
	urnet::ProxyConfig back = j.get<urnet::ProxyConfig>();
	CHECK(back.enable_socks == true && back.enable_http == true);

	// raii handles and std::function callbacks
	char storageDir[] = "/tmp/urnet_smoke_hpp_XXXXXX";
	CHECK(mkdtemp(storageDir) != nullptr);
	{
		urnet::AsyncLocalState asyncLocalState = urnet::newAsyncLocalState(storageDir);
		CHECK(asyncLocalState);
		urnet::LocalState localState = asyncLocalState.getLocalState();
		CHECK(localState);
		(void)localState.getByJwt();

		std::atomic<int> callbackCount{0};
		asyncLocalState.getByJwt([&callbackCount](std::string result, bool ok) {
			(void)result;
			(void)ok;
			callbackCount.fetch_add(1);
		});
		for (int i = 0; i < 500 && callbackCount.load() == 0; i += 1) {
			std::this_thread::sleep_for(std::chrono::milliseconds(10));
		}
		CHECK(callbackCount.load() == 1);

		asyncLocalState.close();
	}
	// raii released every handle
	CHECK(urnet::liveHandleCount() == 0);

	std::printf("OK\n");
	return 0;
}
