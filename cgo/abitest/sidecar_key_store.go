package abitest

/*
#include <errno.h>
#include <pthread.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#define SIDECAR_KEY_STORE_ITEM_COUNT 16
#define SIDECAR_KEY_STORE_NAME_BYTE_COUNT 256
#define SIDECAR_KEY_STORE_VALUE_BYTE_COUNT 1024

// How the store answers: as a store that works, or each way a host's store can go wrong. The statuses are the
// header's: SidecarKeyStoreOk 1, Locked 4 and Failed 0 for a write; SidecarSecretPresent 2, Absent 3, Locked 4 and
// Failed 0 for a read.
enum {
	SIDECAR_KEY_STORE_WORKING = 0,
	SIDECAR_KEY_STORE_LOCKED = 1,
	SIDECAR_KEY_STORE_FAILED = 2,
	// a read answers NULL, as a host that could not build its answer would
	SIDECAR_KEY_STORE_NULL = 3,
	// a read answers text that is no json
	SIDECAR_KEY_STORE_GARBAGE = 4,
	// every answer is a status no header names
	SIDECAR_KEY_STORE_UNKNOWN = 5,
	// a read answers Present with a value that is no base64
	SIDECAR_KEY_STORE_CORRUPT = 6,
};

// A SidecarKeyStore as a C host writes one: named items, its mode, and how many calls each function took and how many
// writes and deletes it made.
typedef struct {
	pthread_mutex_t lock;
	char names[SIDECAR_KEY_STORE_ITEM_COUNT][SIDECAR_KEY_STORE_NAME_BYTE_COUNT];
	char values[SIDECAR_KEY_STORE_ITEM_COUNT][SIDECAR_KEY_STORE_VALUE_BYTE_COUNT];
	int used[SIDECAR_KEY_STORE_ITEM_COUNT];
	int mode;
	int64_t put_count;
	int64_t get_count;
	int64_t delete_count;
	int64_t written_count;
	int64_t deleted_count;
} sidecar_key_store;

static int sidecar_key_store_find(sidecar_key_store* store, const char* name) {
	for (int i = 0; i < SIDECAR_KEY_STORE_ITEM_COUNT; i += 1) {
		if (store->used[i] && strcmp(store->names[i], name) == 0) {
			return i;
		}
	}
	return -1;
}

// What a write answers in a mode that is not working.
static int64_t sidecar_key_store_write_refusal(int mode) {
	switch (mode) {
	case SIDECAR_KEY_STORE_LOCKED:
		return 4;
	case SIDECAR_KEY_STORE_UNKNOWN:
		return 99;
	}
	return 0;
}

static int64_t sidecar_key_store_put_secret(void* user_data, const char* name, const char* value_base64) {
	sidecar_key_store* store = (sidecar_key_store*)user_data;
	pthread_mutex_lock(&store->lock);
	store->put_count += 1;
	int64_t status = 0;
	if (store->mode != SIDECAR_KEY_STORE_WORKING) {
		status = sidecar_key_store_write_refusal(store->mode);
	} else if (name != NULL && value_base64 != NULL && strlen(name) < SIDECAR_KEY_STORE_NAME_BYTE_COUNT && strlen(value_base64) < SIDECAR_KEY_STORE_VALUE_BYTE_COUNT) {
		int at = sidecar_key_store_find(store, name);
		for (int i = 0; at < 0 && i < SIDECAR_KEY_STORE_ITEM_COUNT; i += 1) {
			if (!store->used[i]) {
				at = i;
			}
		}
		if (at >= 0) {
			strcpy(store->names[at], name);
			strcpy(store->values[at], value_base64);
			store->used[at] = 1;
			store->written_count += 1;
			status = 1;
		}
	}
	pthread_mutex_unlock(&store->lock);
	return status;
}

// The answer is malloc'd, as urnet_copy_string's is in a host that links the library: the library frees it.
static char* sidecar_key_store_get_secret(void* user_data, const char* name) {
	sidecar_key_store* store = (sidecar_key_store*)user_data;
	pthread_mutex_lock(&store->lock);
	store->get_count += 1;
	char* answer = NULL;
	switch (store->mode) {
	case SIDECAR_KEY_STORE_LOCKED:
		answer = strdup("{\"status\":4}");
		break;
	case SIDECAR_KEY_STORE_FAILED:
		answer = strdup("{\"status\":0}");
		break;
	case SIDECAR_KEY_STORE_NULL:
		break;
	case SIDECAR_KEY_STORE_GARBAGE:
		answer = strdup("not json");
		break;
	case SIDECAR_KEY_STORE_UNKNOWN:
		answer = strdup("{\"status\":99}");
		break;
	case SIDECAR_KEY_STORE_CORRUPT:
		answer = strdup("{\"status\":2,\"value_base64\":\"not base64!\"}");
		break;
	default: {
		int at = name == NULL ? -1 : sidecar_key_store_find(store, name);
		if (at < 0) {
			answer = strdup("{\"status\":3}");
			break;
		}
		const char* head = "{\"status\":2,\"value_base64\":\"";
		const char* tail = "\"}";
		answer = (char*)malloc(strlen(head) + strlen(store->values[at]) + strlen(tail) + 1);
		if (answer != NULL) {
			strcpy(answer, head);
			strcat(answer, store->values[at]);
			strcat(answer, tail);
		}
	}
	}
	pthread_mutex_unlock(&store->lock);
	return answer;
}

static int64_t sidecar_key_store_delete_secret(void* user_data, const char* name) {
	sidecar_key_store* store = (sidecar_key_store*)user_data;
	pthread_mutex_lock(&store->lock);
	store->delete_count += 1;
	int64_t status = 1;
	if (store->mode != SIDECAR_KEY_STORE_WORKING) {
		status = sidecar_key_store_write_refusal(store->mode);
	} else {
		int at = name == NULL ? -1 : sidecar_key_store_find(store, name);
		if (at >= 0) {
			store->used[at] = 0;
			store->deleted_count += 1;
		}
	}
	pthread_mutex_unlock(&store->lock);
	return status;
}

static sidecar_key_store* sidecar_key_store_new(void) {
	sidecar_key_store* store = (sidecar_key_store*)calloc(1, sizeof(sidecar_key_store));
	if (store != NULL) {
		pthread_mutex_init(&store->lock, NULL);
	}
	return store;
}

static void sidecar_key_store_free(sidecar_key_store* store) {
	pthread_mutex_destroy(&store->lock);
	free(store);
}

static void sidecar_key_store_set_mode(sidecar_key_store* store, int mode) {
	pthread_mutex_lock(&store->lock);
	store->mode = mode;
	pthread_mutex_unlock(&store->lock);
}

// put, get, delete, written, deleted
static void sidecar_key_store_counts(sidecar_key_store* store, int64_t* counts) {
	pthread_mutex_lock(&store->lock);
	counts[0] = store->put_count;
	counts[1] = store->get_count;
	counts[2] = store->delete_count;
	counts[3] = store->written_count;
	counts[4] = store->deleted_count;
	pthread_mutex_unlock(&store->lock);
}

static int sidecar_key_store_item_count(sidecar_key_store* store) {
	pthread_mutex_lock(&store->lock);
	int count = 0;
	for (int i = 0; i < SIDECAR_KEY_STORE_ITEM_COUNT; i += 1) {
		count += store->used[i];
	}
	pthread_mutex_unlock(&store->lock);
	return count;
}

static void* sidecar_key_store_put_secret_pointer(void) { return (void*)sidecar_key_store_put_secret; }
static void* sidecar_key_store_get_secret_pointer(void) { return (void*)sidecar_key_store_get_secret; }
static void* sidecar_key_store_delete_secret_pointer(void) { return (void*)sidecar_key_store_delete_secret; }

// A SidecarPresenceSigner as a C host writes one: it answers a fixed evidence, or NULL, and keeps each message it was
// asked to sign.
typedef struct {
	pthread_mutex_t lock;
	char* answer;
	int answers_null;
	int64_t sign_count;
	char* last_message;
} presence_signer;

static char* presence_signer_get_attestation(void* user_data) {
	return strdup("");
}

static char* presence_signer_sign_with_presence(void* user_data, const char* message_base64) {
	presence_signer* signer = (presence_signer*)user_data;
	pthread_mutex_lock(&signer->lock);
	signer->sign_count += 1;
	free(signer->last_message);
	signer->last_message = message_base64 == NULL ? NULL : strdup(message_base64);
	char* answer = NULL;
	if (!signer->answers_null && signer->answer != NULL) {
		answer = strdup(signer->answer);
	}
	pthread_mutex_unlock(&signer->lock);
	return answer;
}

static presence_signer* presence_signer_new(const char* answer) {
	presence_signer* signer = (presence_signer*)calloc(1, sizeof(presence_signer));
	if (signer != NULL) {
		pthread_mutex_init(&signer->lock, NULL);
		signer->answer = strdup(answer);
	}
	return signer;
}

static void presence_signer_free(presence_signer* signer) {
	pthread_mutex_destroy(&signer->lock);
	free(signer->answer);
	free(signer->last_message);
	free(signer);
}

static void presence_signer_set_answers_null(presence_signer* signer, int answers_null) {
	pthread_mutex_lock(&signer->lock);
	signer->answers_null = answers_null;
	pthread_mutex_unlock(&signer->lock);
}

// The count, and a copy of the last message the caller frees.
static int64_t presence_signer_asked(presence_signer* signer, char** last_message) {
	pthread_mutex_lock(&signer->lock);
	int64_t count = signer->sign_count;
	*last_message = signer->last_message == NULL ? NULL : strdup(signer->last_message);
	pthread_mutex_unlock(&signer->lock);
	return count;
}

static void* presence_signer_get_attestation_pointer(void) { return (void*)presence_signer_get_attestation; }
static void* presence_signer_sign_with_presence_pointer(void) { return (void*)presence_signer_sign_with_presence; }

// A SidecarResultCallback as a C host writes one: each result counted and the first kept, a waiter woken by it.
typedef struct {
	pthread_mutex_t lock;
	pthread_cond_t arrived;
	int64_t count;
	char* first_json;
	char* first_error;
} result_callback;

static void result_callback_result(void* user_data, const char* result_json, const char* err_param) {
	result_callback* callback = (result_callback*)user_data;
	pthread_mutex_lock(&callback->lock);
	callback->count += 1;
	if (callback->count == 1) {
		callback->first_json = result_json == NULL ? NULL : strdup(result_json);
		callback->first_error = err_param == NULL ? NULL : strdup(err_param);
	}
	pthread_cond_broadcast(&callback->arrived);
	pthread_mutex_unlock(&callback->lock);
}

static result_callback* result_callback_new(void) {
	result_callback* callback = (result_callback*)calloc(1, sizeof(result_callback));
	if (callback != NULL) {
		pthread_mutex_init(&callback->lock, NULL);
		pthread_cond_init(&callback->arrived, NULL);
	}
	return callback;
}

static void result_callback_free(result_callback* callback) {
	pthread_cond_destroy(&callback->arrived);
	pthread_mutex_destroy(&callback->lock);
	free(callback->first_json);
	free(callback->first_error);
	free(callback);
}

// Waits until a result arrived or timeout_millis passed: the count then, and copies of the first result's json and
// error, which the caller frees.
static int64_t result_callback_wait(result_callback* callback, int64_t timeout_millis, char** first_json, char** first_error) {
	struct timespec deadline;
	clock_gettime(CLOCK_REALTIME, &deadline);
	deadline.tv_sec += timeout_millis / 1000;
	deadline.tv_nsec += (timeout_millis % 1000) * 1000000;
	if (deadline.tv_nsec >= 1000000000) {
		deadline.tv_sec += 1;
		deadline.tv_nsec -= 1000000000;
	}
	pthread_mutex_lock(&callback->lock);
	while (callback->count == 0) {
		if (pthread_cond_timedwait(&callback->arrived, &callback->lock, &deadline) == ETIMEDOUT) {
			break;
		}
	}
	int64_t count = callback->count;
	*first_json = callback->first_json == NULL ? NULL : strdup(callback->first_json);
	*first_error = callback->first_error == NULL ? NULL : strdup(callback->first_error);
	pthread_mutex_unlock(&callback->lock);
	return count;
}

static int64_t result_callback_count(result_callback* callback) {
	pthread_mutex_lock(&callback->lock);
	int64_t count = callback->count;
	pthread_mutex_unlock(&callback->lock);
	return count;
}

static void* result_callback_result_pointer(void) { return (void*)result_callback_result; }
*/
import "C"

import (
	"time"
	"unsafe"
)

// How a SidecarKeyStore answers: as a working store, or each way a host's store can go wrong.
const (
	SidecarKeyStoreWorking = int(C.SIDECAR_KEY_STORE_WORKING)
	SidecarKeyStoreLocked  = int(C.SIDECAR_KEY_STORE_LOCKED)
	SidecarKeyStoreFailed  = int(C.SIDECAR_KEY_STORE_FAILED)
	SidecarKeyStoreNull    = int(C.SIDECAR_KEY_STORE_NULL)
	SidecarKeyStoreGarbage = int(C.SIDECAR_KEY_STORE_GARBAGE)
	SidecarKeyStoreUnknown = int(C.SIDECAR_KEY_STORE_UNKNOWN)
	SidecarKeyStoreCorrupt = int(C.SIDECAR_KEY_STORE_CORRUPT)
)

// A SidecarKeyStore in C memory, with the three C functions an app passes for it. Safe for calls from any thread.
type SidecarKeyStore struct {
	store *C.sidecar_key_store
}

// A working store holding nothing. Free it once no Sidecar or adapter that was given it can call it.
func NewSidecarKeyStore() *SidecarKeyStore {
	return &SidecarKeyStore{store: C.sidecar_key_store_new()}
}

// Releases the store's memory.
func (self *SidecarKeyStore) Free() {
	C.sidecar_key_store_free(self.store)
	self.store = nil
}

// Sets how the store answers from now on.
func (self *SidecarKeyStore) SetMode(mode int) {
	C.sidecar_key_store_set_mode(self.store, C.int(mode))
}

// The user_data the three functions take: the store itself.
func (self *SidecarKeyStore) UserData() unsafe.Pointer {
	return unsafe.Pointer(self.store)
}

// The C function a host passes as put_secret.
func (self *SidecarKeyStore) PutSecretFunction() unsafe.Pointer {
	return C.sidecar_key_store_put_secret_pointer()
}

// The C function a host passes as get_secret.
func (self *SidecarKeyStore) GetSecretFunction() unsafe.Pointer {
	return C.sidecar_key_store_get_secret_pointer()
}

// The C function a host passes as delete_secret.
func (self *SidecarKeyStore) DeleteSecretFunction() unsafe.Pointer {
	return C.sidecar_key_store_delete_secret_pointer()
}

// What the store took and did: the calls each function took, the items it wrote and deleted, and the items it holds.
type SidecarKeyStoreCounts struct {
	PutCount     int64
	GetCount     int64
	DeleteCount  int64
	WrittenCount int64
	DeletedCount int64
	ItemCount    int
}

// The store's counts now.
func (self *SidecarKeyStore) Counts() SidecarKeyStoreCounts {
	var counts [5]C.int64_t
	C.sidecar_key_store_counts(self.store, &counts[0])
	return SidecarKeyStoreCounts{
		PutCount:     int64(counts[0]),
		GetCount:     int64(counts[1]),
		DeleteCount:  int64(counts[2]),
		WrittenCount: int64(counts[3]),
		DeletedCount: int64(counts[4]),
		ItemCount:    int(C.sidecar_key_store_item_count(self.store)),
	}
}

// A SidecarPresenceSigner in C memory answering a fixed evidence. Safe for calls from any thread.
type PresenceSigner struct {
	signer *C.presence_signer
}

// A signer answering answer, base64 of an evidence, to every message.
func NewPresenceSigner(answer string) *PresenceSigner {
	answerC := C.CString(answer)
	defer C.free(unsafe.Pointer(answerC))
	return &PresenceSigner{signer: C.presence_signer_new(answerC)}
}

// Releases the signer's memory.
func (self *PresenceSigner) Free() {
	C.presence_signer_free(self.signer)
	self.signer = nil
}

// Makes sign_with_presence answer NULL from now on, or stop.
func (self *PresenceSigner) SetAnswersNull(answersNull bool) {
	C.presence_signer_set_answers_null(self.signer, boolInt(answersNull))
}

// The user_data the two functions take: the signer itself.
func (self *PresenceSigner) UserData() unsafe.Pointer {
	return unsafe.Pointer(self.signer)
}

// The C function a host passes as get_attestation.
func (self *PresenceSigner) GetAttestationFunction() unsafe.Pointer {
	return C.presence_signer_get_attestation_pointer()
}

// The C function a host passes as sign_with_presence.
func (self *PresenceSigner) SignWithPresenceFunction() unsafe.Pointer {
	return C.presence_signer_sign_with_presence_pointer()
}

// How many messages it was asked to sign, and the last of them.
func (self *PresenceSigner) Asked() (int64, string) {
	var lastMessage *C.char
	count := C.presence_signer_asked(self.signer, &lastMessage)
	if lastMessage == nil {
		return int64(count), ""
	}
	defer C.free(unsafe.Pointer(lastMessage))
	return int64(count), C.GoString(lastMessage)
}

// A SidecarResultCallback in C memory, counting its results. Safe for calls from any thread.
type ResultCallback struct {
	callback *C.result_callback
}

// A callback with no result yet.
func NewResultCallback() *ResultCallback {
	return &ResultCallback{callback: C.result_callback_new()}
}

// Releases the callback's memory, once nothing can call it.
func (self *ResultCallback) Free() {
	C.result_callback_free(self.callback)
	self.callback = nil
}

// The user_data the function takes: the callback itself.
func (self *ResultCallback) UserData() unsafe.Pointer {
	return unsafe.Pointer(self.callback)
}

// The C function a host passes as the result callback.
func (self *ResultCallback) ResultFunction() unsafe.Pointer {
	return C.result_callback_result_pointer()
}

// Waits for the first result up to timeout: how many results arrived by then, and the first one's json and error.
func (self *ResultCallback) Wait(timeout time.Duration) (count int64, resultJson string, errorText string) {
	var firstJson, firstError *C.char
	count = int64(C.result_callback_wait(self.callback, C.int64_t(timeout.Milliseconds()), &firstJson, &firstError))
	if firstJson != nil {
		resultJson = C.GoString(firstJson)
		C.free(unsafe.Pointer(firstJson))
	}
	if firstError != nil {
		errorText = C.GoString(firstError)
		C.free(unsafe.Pointer(firstError))
	}
	return count, resultJson, errorText
}

// How many results arrived.
func (self *ResultCallback) Count() int64 {
	return int64(C.result_callback_count(self.callback))
}
