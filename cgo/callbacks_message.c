/* HAND-WRITTEN; see callbacks_message.h. */
#include "callbacks_message.h"

void urnet_invoke_message_connect_attempt(urnet_message_connect_attempt_cb cb, void* user_data, int32_t attempt, int64_t elapsed_ms, int64_t backoff_ms, const char* err) {
	cb(user_data, attempt, elapsed_ms, backoff_ms, err);
}
