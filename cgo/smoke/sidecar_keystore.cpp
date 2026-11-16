// The app's key store and presence key through the c abi, as a C++ host writes them (SDK-M8-15; sidecar §22.5's §18
// row, §18.5's SidecarKeyStore and SidecarPresenceSigner, D-SDK-15, D-CORE-15): a local Sidecar opened through
// urnet_new_sidecar_local on a locked store opens locked and creates nothing, and urnet_sidecar_unlock("") answers once
// and makes U; reopened on the same store it opens U again and writes nothing; and on every answer the sdk cannot
// vouch for (a Failed status, NULL, text that is no json, a status no header names, a value that is no base64) it opens
// locked with nothing written. A presence key's answer is the sdk's to read. State changes are waited for on the
// Sidecar's event listener and a condition variable, never by sleeping. No Sidecar here enrolls, so nothing reaches an
// api or reads this host's app stores.
// Build and run with `make smoke_sidecar_keystore` from the cgo module root.

#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <filesystem>
#include <map>
#include <mutex>
#include <string>
#include <unistd.h>

#include "urnetwork_sdk.h"

#define CHECK(cond)                                                              \
	do {                                                                         \
		if (!(cond)) {                                                           \
			std::fprintf(stderr, "FAIL %s:%d: %s\n", __FILE__, __LINE__, #cond); \
			return 1;                                                            \
		}                                                                        \
	} while (0)

// How a store answers: the header's statuses, SidecarKeyStoreOk 1, Locked 4 and Failed 0 for a write, and
// SidecarSecretPresent 2, Absent 3, Locked 4 and Failed 0 for a read.
enum class Mode { working, locked, failed, null_read, not_json, unknown_status, not_base64 };

// A key store as a C++ host writes one: items by name, a mode, and the writes it made.
struct KeyStore {
	std::mutex lock;
	Mode mode = Mode::working;
	std::map<std::string, std::string> items;
	int64_t writes = 0;
	int64_t deletes = 0;
	int64_t reads = 0;
};

static char* copyOf(const std::string& text) {
	char* copy = static_cast<char*>(std::malloc(text.size() + 1));
	if (copy != nullptr) {
		std::memcpy(copy, text.c_str(), text.size() + 1);
	}
	return copy;
}

static int64_t writeRefusal(Mode mode) {
	switch (mode) {
	case Mode::locked:
		return 4;
	case Mode::unknown_status:
		return 99;
	default:
		return 0;
	}
}

static int64_t putSecret(void* userData, const char* name, const char* valueBase64) {
	auto* store = static_cast<KeyStore*>(userData);
	std::lock_guard<std::mutex> guard(store->lock);
	if (store->mode != Mode::working) {
		return writeRefusal(store->mode);
	}
	store->items[name] = valueBase64;
	store->writes += 1;
	return 1;
}

// The answer is malloc'd: the library frees it.
static char* getSecret(void* userData, const char* name) {
	auto* store = static_cast<KeyStore*>(userData);
	std::lock_guard<std::mutex> guard(store->lock);
	store->reads += 1;
	switch (store->mode) {
	case Mode::locked:
		return copyOf("{\"status\":4}");
	case Mode::failed:
		return copyOf("{\"status\":0}");
	case Mode::null_read:
		return nullptr;
	case Mode::not_json:
		return copyOf("not json");
	case Mode::unknown_status:
		return copyOf("{\"status\":99}");
	case Mode::not_base64:
		return copyOf("{\"status\":2,\"value_base64\":\"not base64!\"}");
	case Mode::working:
		break;
	}
	auto item = store->items.find(name);
	if (item == store->items.end()) {
		return copyOf("{\"status\":3}");
	}
	return copyOf(std::string("{\"status\":2,\"value_base64\":\"") + item->second + "\"}");
}

static int64_t deleteSecret(void* userData, const char* name) {
	auto* store = static_cast<KeyStore*>(userData);
	std::lock_guard<std::mutex> guard(store->lock);
	if (store->mode != Mode::working) {
		return writeRefusal(store->mode);
	}
	if (store->items.erase(name) != 0) {
		store->deletes += 1;
	}
	return 1;
}

// A presence key that the person refuses: NULL, which the sdk reads as refused.
static char* getAttestation(void*) {
	return copyOf("");
}

static char* signWithPresence(void*, const char*) {
	return nullptr;
}

// State events, counted, a waiter woken by each.
struct Events {
	std::mutex lock;
	std::condition_variable arrived;
	int64_t count = 0;
};

static void onEvent(void* userData, const char*) {
	auto* events = static_cast<Events*>(userData);
	std::lock_guard<std::mutex> guard(events->lock);
	events->count += 1;
	events->arrived.notify_all();
}

// Waits until the Sidecar's state is want, reading it again at each event, up to a bound far past any answer here.
static bool awaitState(uint64_t sidecar, Events& events, const char* want) {
	auto deadline = std::chrono::steady_clock::now() + std::chrono::seconds(30);
	while (true) {
		int64_t seen;
		{
			std::lock_guard<std::mutex> guard(events.lock);
			seen = events.count;
		}
		char* state = urnet_sidecar_get_state(sidecar);
		bool reached = state != nullptr && std::strcmp(state, want) == 0;
		urnet_free_string(state);
		if (reached) {
			return true;
		}
		std::unique_lock<std::mutex> guard(events.lock);
		if (!events.arrived.wait_until(guard, deadline, [&events, seen] { return events.count != seen; })) {
			return false;
		}
	}
}

// One result, signaled when it arrives, and how many came.
struct Answer {
	std::mutex lock;
	std::condition_variable arrived;
	int64_t count = 0;
	std::string json;
};

static void onAnswer(void* userData, const char* resultJson, const char*) {
	auto* answer = static_cast<Answer*>(userData);
	std::lock_guard<std::mutex> guard(answer->lock);
	answer->count += 1;
	if (resultJson != nullptr && answer->count == 1) {
		answer->json = resultJson;
	}
	answer->arrived.notify_all();
}

static bool await(Answer& answer) {
	std::unique_lock<std::mutex> guard(answer.lock);
	return answer.arrived.wait_for(guard, std::chrono::seconds(30), [&answer] { return answer.count != 0; });
}

// A local Sidecar on store and stateDir, with its event listener: 0 if none opened.
static uint64_t open(uint64_t space, KeyStore& store, Events& events, const std::string& stateDir, uint64_t* sub) {
	std::string settings = std::string("{\"state_dir\":\"") + stateDir + "\"}";
	char* error = nullptr;
	uint64_t sidecar = urnet_new_sidecar_local(space, deleteSecret, getSecret, putSecret, &store, getAttestation, signWithPresence, nullptr, settings.c_str(), &error);
	if (error != nullptr) {
		std::fprintf(stderr, "open: %s\n", error);
		urnet_free_string(error);
	}
	if (sidecar != 0) {
		*sub = urnet_sidecar_add_event_listener(sidecar, onEvent, &events);
	}
	return sidecar;
}

static void close(uint64_t sidecar, uint64_t sub) {
	urnet_sub_close(sub);
	urnet_release(sub);
	urnet_sidecar_close(sidecar);
	urnet_release(sidecar);
}

static std::string newStateDir() {
	char dir[] = "/tmp/urnet_sidecar_keystore_XXXXXX";
	return mkdtemp(dir) == nullptr ? std::string() : std::string(dir);
}

int main() {
	CHECK(urnet_live_handle_count() == 0);
	uint64_t manager = urnet_new_network_space_manager_no_storage();
	CHECK(manager != 0);
	uint64_t space = urnet_network_space_manager_update_network_space_values(
		manager, "{\"host_name\":\"sdktest.example\",\"env_name\":\"test\"}", "{}");
	CHECK(space != 0);

	// locked: nothing written until the unlock, which answers once and makes U
	KeyStore store;
	store.mode = Mode::locked;
	Events events;
	std::string stateDir = newStateDir();
	CHECK(!stateDir.empty());
	uint64_t sub = 0;
	uint64_t sidecar = open(space, store, events, stateDir, &sub);
	CHECK(sidecar != 0);
	CHECK(awaitState(sidecar, events, "locked"));
	{
		std::lock_guard<std::mutex> guard(store.lock);
		CHECK(store.writes == 0 && store.items.empty());
		store.mode = Mode::working;
	}
	Answer unlocked;
	urnet_sidecar_unlock(sidecar, "", onAnswer, &unlocked);
	CHECK(await(unlocked));
	CHECK(std::strstr(unlocked.json.c_str(), "\"ok\":true") != nullptr);
	CHECK(awaitState(sidecar, events, "unenrolled"));
	{
		std::lock_guard<std::mutex> guard(store.lock);
		CHECK(store.writes == 1 && store.items.size() == 1);
	}
	close(sidecar, sub);

	// reopened on the same store and state: U opens and nothing is written
	Events reopenedEvents;
	sidecar = open(space, store, reopenedEvents, stateDir, &sub);
	CHECK(sidecar != 0);
	CHECK(awaitState(sidecar, reopenedEvents, "unenrolled"));
	{
		std::lock_guard<std::mutex> guard(store.lock);
		CHECK(store.writes == 1 && store.deletes == 0 && store.items.size() == 1);
	}
	close(sidecar, sub);

	// every answer the sdk cannot vouch for: locked, nothing written
	for (Mode mode : {Mode::failed, Mode::null_read, Mode::not_json, Mode::unknown_status, Mode::not_base64}) {
		KeyStore failing;
		failing.mode = mode;
		Events failingEvents;
		std::string failingDir = newStateDir();
		CHECK(!failingDir.empty());
		uint64_t failingSub = 0;
		uint64_t failingSidecar = open(space, failing, failingEvents, failingDir, &failingSub);
		CHECK(failingSidecar != 0);
		CHECK(awaitState(failingSidecar, failingEvents, "locked"));
		{
			std::lock_guard<std::mutex> guard(failing.lock);
			CHECK(failing.reads != 0 && failing.writes == 0 && failing.deletes == 0);
		}
		close(failingSidecar, failingSub);
		std::filesystem::remove_all(failingDir);
	}
	{
		std::lock_guard<std::mutex> guard(unlocked.lock);
		CHECK(unlocked.count == 1);
	}

	std::filesystem::remove_all(stateDir);
	CHECK(urnet_release(space));
	urnet_network_space_manager_close(manager);
	CHECK(urnet_release(manager));
	std::printf("OK\n");
	return 0;
}
