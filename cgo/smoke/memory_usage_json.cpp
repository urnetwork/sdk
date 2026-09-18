// Exercise the shipped C++ memory snapshot through its actual JSON conversions.
// Run with `make smoke_memory_usage_json`; no SDK library or device is needed.
#include "urnetwork_sdk.hpp"

#include <cstdio>

// Distinct nonzero values expose omitted or cross-wired fields. Legacy fields
// remain controls, and the handoff id checks integer precision above 2^53.
int main() {
	using Json = nlohmann::json;
	const Json expected = {
		{"PeerKeyPinBudgetByteCount", 101},
		{"PeerKeyPinUsedByteCount", 102},
		{"PeerKeyPinReservedByteCount", 103},
		{"PeerKeyPinReleasedByteCount", 104},
		{"PeerKeyPinCount", 105},
		{"PeerKeyPinCapacityRefusals", 106},
		{"PeerKeyPinPersistenceFailures", 107},
		{"PeerKeyPinRollbackRefusals", 108},
		{"PeerKeyPinStateFailures", 109},
		{"TransferRootBudgetByteCount", 110},
		{"TransferRootUsedByteCount", 111},
		{"TransferRootReservedByteCount", 112},
		{"TransferRootReleasedByteCount", 113},
		{"ClientTransferBudgetByteCount", 114},
		{"ClientTransferUsedByteCount", 115},
		{"ProviderTransferBudgetByteCount", 116},
		{"ProviderTransferUsedByteCount", 117},
		{"NatBudgetByteCount", 118},
		{"NatUsedByteCount", 119},
		{"NatReservedByteCount", 120},
		{"NatReleasedByteCount", 121},
		{"PlatformTransportReservedBytes", 122},
		{"PlatformTransportReleasedBytes", 123},
		{"PlatformTransportHandoffID", 9007199254740993LL},
		{"PlatformTransportHandoffFromClass", "h3"},
		{"PlatformTransportHandoffToClass", "h1"},
		{"PlatformTransportHandoffH1ByteCount", 124},
		{"TargetByteCount", 125},
		{"TotalByteCount", 126},
		{"ProviderWindowKnown", true},
		{"ProviderWindowMinSatisfied", false},
	};
	const auto usage = Json::parse(expected.dump()).get<urnet::DeviceLocalMemoryUsage>();
	const Json roundTrip = Json::parse(Json(usage).dump());
	for (const auto& field : expected.items()) {
		const auto actual = roundTrip.find(field.key());
		if (actual == roundTrip.end() || *actual != field.value()) {
			std::fprintf(stderr, "memory snapshot lost JSON field %s\n", field.key().c_str());
			return 1;
		}
	}

	// Old libraries omit the new telemetry. Their sparse snapshots must still
	// decode, with legacy values retained and the new members zero-initialized.
	const Json legacy = {{"TargetByteCount", 201}, {"ProviderWindowKnown", true}};
	const Json decodedLegacy = legacy.get<urnet::DeviceLocalMemoryUsage>();
	for (const auto& field : expected.items()) {
		const auto original = legacy.find(field.key());
		const Json expectedValue = original != legacy.end() ? *original :
			field.value().is_string() ? Json("") :
			field.value().is_boolean() ? Json(false) : Json(0);
		const auto actual = decodedLegacy.find(field.key());
		if (actual == decodedLegacy.end() || *actual != expectedValue) {
			std::fprintf(stderr, "legacy memory snapshot changed JSON field %s\n", field.key().c_str());
			return 1;
		}
	}
	std::puts("memory snapshot JSON round-trip and legacy defaults passed");
	return 0;
}
