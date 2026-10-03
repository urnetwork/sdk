// Exercise the shipped C++ request and result without an SDK library or device.
#include "urnetwork_sdk.hpp"

#include <cstdio>

int main() {
	using Json = nlohmann::json;
	const Json expected = {
		{"request_id", "00000000-0000-4000-8000-000000000001"},
		{"to_address", "synthetic-destination"},
		{"amount_usdc_nano_cents", 9223372036854775000LL},
		{"terms", true},
	};
	const auto intent = Json::parse(expected.dump()).get<urnet::WalletCircleTransferOutArgs>();
	const Json restored = Json::parse(Json(intent).dump());
	if (restored != expected) {
		std::fputs("customer transfer lost persisted intent or exact int64 amount\n", stderr);
		return 1;
	}
	const Json outcome = {
		{"request_id", expected["request_id"]},
		{"challenge_id", "00000000-0000-4000-8000-000000000002"},
		{"challenge_status", "COMPLETE"},
	};
	const Json result = outcome.get<urnet::WalletCircleTransferOutResult>();
	for (const auto& field : outcome.items()) {
		if (!result.contains(field.key()) || result[field.key()] != field.value()) {
			std::fputs("customer result lost original request or challenge status\n", stderr);
			return 1;
		}
	}
	const Json legacy = {{"to_address", "synthetic-destination"}, {"amount_usdc_nano_cents", 1000}, {"terms", true}};
	const auto absent = legacy.get<urnet::WalletCircleTransferOutArgs>();
	if (absent.request_id) {
		std::fputs("legacy JSON fabricated a new caller request id\n", stderr);
		return 1;
	}
	std::puts("customer intent and challenge JSON round-trip passed");
	return 0;
}
