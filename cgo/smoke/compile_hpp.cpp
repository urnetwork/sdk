// Compile-only check of the generated c++ wrapper. No unistd.h, no runtime, no
// link step -- it exists to be compilable by ANY c++20 toolchain, msvc
// included, because the wrapper's consumers are not all posix (the windows
// client embeds it through the c abi, while android and apple go through
// gomobile and never compile this header at all).
//
// It pins the shapes a generator change can silently break: every trampoline
// must convert to the c abi callback typedef it is passed as. A string
// returning callback is the interesting one -- the c++ side returns
// std::string while the abi wants char*, and getting that wrong produced a
// header that could not compile itself, which nothing noticed until the
// windows client became the first consumer to build it.
//
// build (msvc):  cl /c /EHsc /std:c++20 /I ../include /I <nlohmann> compile_hpp.cpp
// build (clang): clang++ -fsyntax-only -std=c++20 -I ../include -I <nlohmann> compile_hpp.cpp

#include "urnetwork_sdk.hpp"

namespace {

// string-returning callback: the trampoline must hand back a malloc'd char*
// that go frees with urnet_free_string
urnet_flow_owner_lookup_cb retained_flow_owner = &urnet::detail::retained_flow_owner_lookup;
urnet_flow_owner_lookup_cb oneshot_flow_owner = &urnet::detail::oneshot_flow_owner_lookup;

void use_flow_owner_lookup(const urnet::DeviceLocal& device) {
	urnet::FlowOwnerLookup lookup =
		[](int64_t version, int64_t protocol, std::string source_ip, int64_t source_port,
		   std::string destination_ip, int64_t destination_port) -> std::string {
			(void)version;
			(void)protocol;
			(void)source_ip;
			(void)source_port;
			(void)destination_ip;
			(void)destination_port;
			return "com.example.app";
		};
	device.setFlowOwnerLookup(lookup);
	device.setFlowOwnerLookup(nullptr);
}

// silence unused warnings without giving the symbols external linkage
void reference_everything(const urnet::DeviceLocal& device) {
	(void)retained_flow_owner;
	(void)oneshot_flow_owner;
	use_flow_owner_lookup(device);
}

} // namespace
