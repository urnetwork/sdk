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

#include <type_traits>
#include <utility>

namespace {

// string-returning callback: the trampoline must hand back a malloc'd char*
// that go frees with urnet_free_string
urnet_flow_owner_lookup_cb retained_flow_owner = &urnet::detail::retained_flow_owner_lookup;
urnet_flow_owner_lookup_cb oneshot_flow_owner = &urnet::detail::oneshot_flow_owner_lookup;

// Private observation state must be represented by move-only owned handles,
// including the result delivered through the save-listener trampoline.
static_assert(std::is_move_constructible_v<urnet::LocalAuthStateSnapshot>);
static_assert(!std::is_copy_constructible_v<urnet::LocalAuthStateSnapshot>);
static_assert(std::is_move_constructible_v<urnet::LocalStateResetResult>);
static_assert(!std::is_copy_constructible_v<urnet::LocalStateResetResult>);
static_assert(std::is_move_constructible_v<urnet::DeviceLocalLoadResult>);
static_assert(!std::is_copy_constructible_v<urnet::DeviceLocalLoadResult>);
static_assert(std::is_move_constructible_v<urnet::DeviceLocalSaveResult>);
static_assert(!std::is_copy_constructible_v<urnet::DeviceLocalSaveResult>);
urnet_local_state_save_cb retained_local_state_save = &urnet::detail::retained_local_state_save;
urnet_local_state_save_cb oneshot_local_state_save = &urnet::detail::oneshot_local_state_save;

// Explicit key saves retain bool/error C results and throwing void C++ methods.
// The checked secret loader is the existing optional JSON list, not a handle.
static_assert(std::is_same_v<decltype(&urnet_device_local_save_key_material), bool (*)(uint64_t, char**)>);
static_assert(std::is_same_v<decltype(&urnet_device_local_save_provide_secret_keys), bool (*)(uint64_t, char**)>);
static_assert(std::is_same_v<decltype(&urnet_local_state_load_provide_secret_keys), char* (*)(uint64_t, char**)>);
static_assert(std::is_same_v<decltype(std::declval<const urnet::DeviceLocal&>().saveKeyMaterial()), void>);
static_assert(std::is_same_v<decltype(std::declval<const urnet::DeviceLocal&>().saveProvideSecretKeys()), void>);
static_assert(std::is_same_v<decltype(std::declval<const urnet::LocalState&>().loadProvideSecretKeys()),
                             std::optional<urnet::ProvideSecretKeyList>>);
static_assert(std::is_same_v<urnet::ProvideSecretKeyList, std::vector<urnet::ProvideSecretKey>>);

// Compile each explicit operation without joining their independent policies.
// This function is never invoked; actual storage semantics have Go controls.
void use_checked_provider_keys(const urnet::LocalState& local_state,
                               const urnet::DeviceLocal& device) {
	try {
		std::optional<urnet::ProvideSecretKeyList> secrets = local_state.loadProvideSecretKeys();
		if (secrets) {
			const bool empty = secrets->empty();
			(void)empty;
		}
		device.saveKeyMaterial();
		device.saveProvideSecretKeys();
	} catch (const urnet::Error& error) {
		(void)error;
	}
	// A present legacy JSON null document parses as an empty list; only a null
	// returned C pointer denotes absence. No format conversion is introduced.
	urnet::ProvideSecretKeyList legacy_null = urnet::detail::parseJson<urnet::ProvideSecretKeyList>("null");
	(void)legacy_null;
}

// Compile the actual caller shapes without constructing a device or touching a
// machine store. Fresh SDK binding/runtime qualification is a separate gate.
void use_preference_observations(const urnet::NetworkSpace& space,
                                const urnet::LocalState& local_state,
                                const urnet::DeviceLocal& device) {
	urnet::LocalAuthStateSnapshot snapshot = space.getAuthStateSnapshot();
	urnet::LocalAuthStateSnapshot disk_only = local_state.getAuthStateSnapshot();
	(void)disk_only;
	(void)snapshot.getEmpty();
	(void)snapshot.getInstanceId();
	(void)snapshot.getByJwt();
	(void)snapshot.getByClientJwt();
	(void)snapshot.parseByJwt();
	std::optional<urnet::ConnectLocation> current = snapshot.loadConnectLocation();
	std::optional<urnet::ConnectLocation> default_location = snapshot.loadDefaultLocation();
	snapshot.setConnectLocation(current);
	snapshot.setDefaultLocation(default_location);
	urnet::LocalStateResetResult reset = space.resetLocalStateIfCurrent(snapshot);
	(void)reset.getReset();
	urnet::DeviceLocalKeyMaterial preserved = reset.getDeviceLocalKeyMaterial();
	(void)preserved;

	urnet::DeviceLocalLoadResult loaded = device.load();
	(void)loaded.getLoaded();
	(void)loaded.getHasConnectLocation();
	(void)loaded.getHasDefaultLocation();
	(void)loaded.getDefaultError();
	(void)loaded.getHasPreference("route-local");
	(void)loaded.getPreferenceError("route-local");
	device.setAutoSave(true);
	(void)device.getAutoSave();
	device.setConnectLocationChecked(current);
	device.setDefaultLocationChecked(default_location);
	device.reconnectChecked(current);
	device.setConnectLocationChecked(std::nullopt);
	device.setDefaultLocationChecked(std::nullopt);

	urnet::DeviceLocalSaveResult last = device.getLastLocalStateSaveResult();
	if (last) {
		(void)last.getSequence();
		(void)last.getPreference();
		(void)last.getAutoSaveEnabled();
		(void)last.getSaved();
		(void)last.getError();
	}
	urnet::Sub listener = device.addLocalStateSaveListener([](urnet::DeviceLocalSaveResult result) {
		if (!result) return;
		const int64_t sequence = result.getSequence();
		const bool saved = result.getSaved();
		const bool enabled = result.getAutoSaveEnabled();
		const std::string preference = result.getPreference();
		const std::string error = result.getError();
		(void)sequence;
		(void)saved;
		(void)enabled;
		(void)preference;
		(void)error;
	});
	(void)listener;
}

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
	(void)retained_local_state_save;
	(void)oneshot_local_state_save;
	(void)&use_preference_observations;
	(void)&use_checked_provider_keys;
	use_flow_owner_lookup(device);
	use_advanced_mode(remote);
}

} // namespace
