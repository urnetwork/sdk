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
	CHECK(urnet::getDefaultTunnelDnsAddressIpv4() == "65.49.70.65");

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

	// packet-batch app bridge types compile through the generated C++ surface;
	// the callback receives a borrowed handle and copies only packets the app
	// actually consumes.
	urnet::ReceivePackets receivePackets = [](urnet::PacketBatch packetBatch) {
		for (int64_t packetIndex = 0; packetIndex < packetBatch.len();
			 packetIndex += 1) {
			(void)packetBatch.ipVersion(packetIndex);
			(void)packetBatch.ipProtocol(packetIndex);
			(void)packetBatch.get(packetIndex);
		}
	};
	CHECK(static_cast<bool>(receivePackets));
	urnet::ReceivePacketBatch receivePacketBatch = [](
		const uint8_t* packetBatchBytes,
		int32_t packetBatchByteCount) {
		(void)packetBatchBytes;
		(void)packetBatchByteCount;
	};
	CHECK(static_cast<bool>(receivePacketBatch));

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

	// The actual paired snapshot/reset boundary uses only a fresh temporary
	// NetworkSpace and loopback configuration. No device, RPC, app or VPN starts.
	char observationDir[] = "/tmp/urnet_observation_hpp_XXXXXX";
	CHECK(mkdtemp(observationDir) != nullptr);
	{
		auto manager = urnet::newNetworkSpaceManager(observationDir);
		urnet::NetworkSpaceKey key{};
		key.host_name = "binding-observation.test";
		key.env_name = "test";
		urnet::NetworkSpaceValues values{};
		values.api_url = "http://127.0.0.1:1";
		values.platform_url = "ws://127.0.0.1:1";
		auto space = manager.updateNetworkSpaceValues(key, values);
		CHECK(space);
		auto state = space.getAsyncLocalState().getLocalState();
		CHECK(state);
		auto original = space.getAuthStateSnapshot();
		CHECK(original && original.getEmpty());
		CHECK(original.getInstanceId().empty());
		CHECK(original.getByJwt().empty() && original.getByClientJwt().empty());

		// A disk-only or null snapshot cannot manufacture paired authority.
		bool wrongOriginThrew = false;
		try {
			auto diskOnly = state.getAuthStateSnapshot();
			(void)space.resetLocalStateIfCurrent(diskOnly);
		} catch (const urnet::Error&) {
			wrongOriginThrew = true;
		}
		CHECK(wrongOriginThrew);
		bool nullOriginThrew = false;
		try {
			(void)space.resetLocalStateIfCurrent(urnet::LocalAuthStateSnapshot{});
		} catch (const urnet::Error&) {
			nullOriginThrew = true;
		}
		CHECK(nullOriginThrew);

		const std::vector<uint8_t> seed(32, 0x6f);
		auto material = urnet::newDeviceLocalKeyMaterial(
			seed.data(), static_cast<int32_t>(seed.size()), nullptr, 0, nullptr, 0);
		state.setDeviceLocalKeyMaterial(material);
		// Synthetic local data only; no API call or credential is involved.
		state.setByJwt("binding-smoke-admin-marker");
		CHECK(original.getEmpty() && original.getByJwt().empty());
		auto superseded = space.resetLocalStateIfCurrent(original);
		CHECK(superseded && !superseded.getReset());
		CHECK(!superseded.getDeviceLocalKeyMaterial());
		CHECK(state.getByJwt() == "binding-smoke-admin-marker");

		auto current = space.getAuthStateSnapshot();
		CHECK(current && !current.getEmpty());
		CHECK(current.getByJwt() == "binding-smoke-admin-marker");
		auto reset = space.resetLocalStateIfCurrent(current);
		CHECK(reset && reset.getReset());
		auto kept = reset.getDeviceLocalKeyMaterial();
		CHECK(kept && kept.getClientKeySeed() == seed);
		CHECK(kept.getProvideTlsCertificatePem().empty());
		CHECK(kept.getProvideTlsPrivateKeyPem().empty());
		CHECK(state.getByJwt().empty());
		CHECK(space.getAuthStateSnapshot().getEmpty());
		CHECK(current.getByJwt() == "binding-smoke-admin-marker");
		manager.close();
	}
	CHECK(urnet::liveHandleCount() == 0);

	std::printf("OK\n");
	return 0;
}
