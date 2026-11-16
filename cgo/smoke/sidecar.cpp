// sdk.Sidecar's c abi smoke test (SDK-M8-06; sidecar §18.5's Gate table): one call on each of its handle types against
// the built library, as a desktop app with no ur daemon running sees it. A remote Sidecar opens offline; its approvals
// list nothing they could not read, answer a revocation's preview and a revocation UNAVAILABLE, and its mail claims no
// indexer and checks and reads no send; a handle the offline daemon gives none of is 0, and a call on 0 is a refusal,
// never a crash; and once every handle is released none is live. Callbacks are waited for on a condition variable.
// Build and run with `make smoke_sidecar` from the cgo module root.

#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
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

// One callback's answer, signaled when it arrives.
struct Answer {
	std::mutex lock;
	std::condition_variable arrived;
	bool done = false;
	std::string json;
	std::string error;
};

static void onAnswer(void* userData, const char* resultJson, const char* err) {
	auto* answer = static_cast<Answer*>(userData);
	std::lock_guard<std::mutex> guard(answer->lock);
	if (resultJson != nullptr) {
		answer->json = resultJson;
	}
	if (err != nullptr) {
		answer->error = err;
	}
	answer->done = true;
	answer->arrived.notify_all();
}

// Waits for the answer, up to a bound the calls' own timeouts are well inside.
static bool await(Answer& answer) {
	std::unique_lock<std::mutex> guard(answer.lock);
	return answer.arrived.wait_for(guard, std::chrono::seconds(60), [&answer] { return answer.done; });
}

static bool contains(const char* text, const char* part) {
	return text != nullptr && std::strstr(text, part) != nullptr;
}

int main() {
	CHECK(urnet_live_handle_count() == 0);

	// the defaults cross as json, and a remote Sidecar opens on an app-group directory where no daemon listens
	char* defaults = urnet_default_sidecar_settings();
	CHECK(contains(defaults, "\"instance\":\"default\""));
	urnet_free_string(defaults);
	char socketDir[] = "/tmp/urnet_sidecar_smoke_XXXXXX";
	CHECK(mkdtemp(socketDir) != nullptr);
	std::string settings = std::string("{\"daemon_socket_dir\":\"") + socketDir + "\"}";
	char* error = nullptr;
	uint64_t sidecar = urnet_new_sidecar_remote(nullptr, nullptr, nullptr, settings.c_str(), &error);
	CHECK(sidecar != 0 && error == nullptr);
	char* state = urnet_sidecar_get_state(sidecar);
	CHECK(state != nullptr && std::strcmp(state, "offline") == 0);
	urnet_free_string(state);

	// SidecarApprovals: nothing read is nothing listed, and a revocation reaches no daemon
	uint64_t approvals = urnet_sidecar_get_approvals(sidecar);
	CHECK(approvals != 0);
	char* devices = urnet_sidecar_approvals_get_devices(approvals);
	CHECK(devices == nullptr);
	char* pending = urnet_sidecar_approvals_get_pending(approvals);
	CHECK(pending == nullptr);
	Answer preview;
	urnet_sidecar_approvals_preview_revoke_device(approvals, "018f2d5e-7b1a-7c3d-9e4f-0a1b2c3d4e60", "lost", 0, 0, onAnswer, &preview);
	CHECK(await(preview));
	CHECK(contains(preview.json.c_str(), "\"code\":\"UNAVAILABLE\""));
	Answer revoke;
	urnet_sidecar_approvals_revoke_device(approvals, "018f2d5e-7b1a-7c3d-9e4f-0a1b2c3d4e61", "[]", onAnswer, &revoke);
	CHECK(await(revoke));
	CHECK(contains(revoke.json.c_str(), "\"ok\":false") && contains(revoke.json.c_str(), "\"code\":\"UNAVAILABLE\""));

	// SidecarMail: no daemon answers, so it claims no indexer
	uint64_t mail = urnet_sidecar_get_mail(sidecar);
	CHECK(mail != 0);
	char* coverage = urnet_sidecar_mail_get_receiver_coverage(mail);
	CHECK(coverage != nullptr && std::strcmp(coverage, "foreground-only") == 0);
	urnet_free_string(coverage);

	// CheckSend and GetSendResult (SDK-M9-01) take a json draft and a send_id and answer through their callbacks: no
	// daemon method checks or reads a send yet, and a draft with no recipient is refused before any backend
	Answer check;
	urnet_sidecar_mail_check_send(mail, "{\"from\":\"admin@net.example\",\"to\":[\"someone@plain.example\"]}", onAnswer, &check);
	CHECK(await(check));
	CHECK(contains(check.json.c_str(), "\"ok\":false") && contains(check.json.c_str(), "\"code\":\"MAIL_UNAVAILABLE\""));
	Answer noRecipient;
	urnet_sidecar_mail_check_send(mail, "{\"from\":\"admin@net.example\"}", onAnswer, &noRecipient);
	CHECK(await(noRecipient));
	CHECK(contains(noRecipient.json.c_str(), "\"code\":\"MAIL_INVALID\""));
	Answer sendResult;
	urnet_sidecar_mail_get_send_result(mail, "018f2d5e-7b1a-7c3d-9e4f-0a1b2c3d4e64", onAnswer, &sendResult);
	CHECK(await(sendResult));
	CHECK(contains(sendResult.json.c_str(), "\"ok\":false") && contains(sendResult.json.c_str(), "\"code\":\"MAIL_UNAVAILABLE\""));

	// SidecarEvs, SidecarSockets and SidecarDirectory: the offline daemon gives none, and a call on 0 refuses
	CHECK(urnet_sidecar_get_evs(sidecar) == 0);
	CHECK(urnet_sidecar_get_sockets(sidecar) == 0);
	CHECK(urnet_sidecar_get_directory(sidecar) == 0);
	Answer unheard;
	urnet_sidecar_evs_read_head(0, "018f2d5e-7b1a-7c3d-9e4f-0a1b2c3d4e63", "a/path", onAnswer, &unheard);
	// an unknown handle returns before the call, so nothing answers
	CHECK(!unheard.done);
	char* listenError = nullptr;
	CHECK(urnet_sidecar_sockets_listen(0, "service:1", 1, &listenError) == 0);
	if (listenError != nullptr) {
		urnet_free_string(listenError);
	}
	char* closeError = nullptr;
	CHECK(!urnet_sidecar_socket_listener_close(0, &closeError));
	if (closeError != nullptr) {
		urnet_free_string(closeError);
	}
	char* requests = urnet_sidecar_directory_get_requests(0, "incoming");
	CHECK(requests == nullptr);

	// every handle released, none live
	urnet_sidecar_close(sidecar);
	CHECK(urnet_release(mail));
	CHECK(urnet_release(approvals));
	CHECK(urnet_release(sidecar));
	CHECK(!urnet_release(sidecar));
	CHECK(urnet_live_handle_count() == 0);
	rmdir(socketDir);

	std::printf("OK\n");
	return 0;
}
