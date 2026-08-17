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

// Advanced mode drives the sdk from a DeviceRemote, because the windows
// client's device lives in a separate service process. These are the controls
// that reach it: fault injection, the probe suite, and the two calls whose
// counts are the only feedback a "requested" button can show.
//
// Pinned here because a return type is part of the c abi: migrateExit and
// probeAllExits were void and now yield counts, and a generator that dropped
// the value again would still produce a header that compiles on its own --
// only a caller that USES the result notices.
void use_advanced_mode(const urnet::DeviceRemote& device) {
	const std::string exit_client_id = "00000000-0000-0000-0000-000000000000";

	// counts, not void: assigning to an integer fails if either regresses
	int64_t migrated = device.migrateExit(exit_client_id);
	int64_t probes_scheduled = device.probeAllExits();
	(void)migrated;
	(void)probes_scheduled;

	// fault injection
	bool dropped = device.dropExit(exit_client_id);
	bool stalled = device.stallExit(exit_client_id, true);
	bool unstalled = device.stallExit(exit_client_id, false);
	(void)dropped;
	(void)stalled;
	(void)unstalled;

	// both spellings of "replace every exit at once" must stay callable on a
	// DeviceRemote: shuffle is the queued legacy action, shuffleExits is the
	// non-queued fault injection one, and they differ only on failure
	device.shuffle();
	device.shuffleExits();

	// probe suite: a config-taking start, a poll, and a list getter
	urnet::ProbeSuiteConfig config{};
	config.Concurrency = 4;
	config.TimeoutMillis = 15000;
	config.RepeatCount = 1;
	config.IncludeDns = true;
	config.IncludeHttp = true;
	config.IncludeDownload = true;
	config.DownloadByteCount = 1 << 20;

	bool started = device.startProbeSuite(config);
	// nullopt is "use the sdk default", which must stay expressible
	bool started_default = device.startProbeSuite(std::nullopt);
	bool running = device.probeSuiteRunning();
	(void)started;
	(void)started_default;
	(void)running;

	// the list getter this whole exercise exists to make usable. Instantiating
	// it compiles detail::parseJson<ProbeResultList>, including the branch that
	// turns a `null` document into an empty container -- which only compiles if
	// the container is default constructible. Every other parseJson<T> in the
	// header is instantiated too, simply by including it, since these are
	// non-template inline members.
	std::optional<urnet::ProbeResultList> results = device.getProbeResults();
	if (results) {
		for (const urnet::ProbeResult& result : *results) {
			(void)result.Name;
			(void)result.Kind;
			(void)result.Ok;
			(void)result.TotalMillis;
		}
	}

	device.stopProbeSuite();
}

// silence unused warnings without giving the symbols external linkage
void reference_everything(const urnet::DeviceLocal& device, const urnet::DeviceRemote& remote) {
	(void)retained_flow_owner;
	(void)oneshot_flow_owner;
	use_flow_owner_lookup(device);
	use_advanced_mode(remote);
}

} // namespace
