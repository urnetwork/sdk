// Package abitest holds C functions the c abi's tests hand to the library as an app would. A Go test file cannot
// import C, and a callback crosses the abi only as a C function pointer, so the C side of a callback test lives here,
// in a package only test files import: nothing of it is linked into the library.
package abitest

/*
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#define FORWARD_KEY_STORE_ITEM_COUNT 16
#define FORWARD_KEY_STORE_NAME_BYTE_COUNT 256
#define FORWARD_KEY_STORE_VALUE_BYTE_COUNT 1024

// A SidecarForwardKeyStore as a C host writes one: a few named items, and how many calls each function took.
// The statuses are the header's: Ok 1 and Failed 0 for a write, Present 2 and Absent 3 for a read.
typedef struct {
	char names[FORWARD_KEY_STORE_ITEM_COUNT][FORWARD_KEY_STORE_NAME_BYTE_COUNT];
	char values[FORWARD_KEY_STORE_ITEM_COUNT][FORWARD_KEY_STORE_VALUE_BYTE_COUNT];
	int used[FORWARD_KEY_STORE_ITEM_COUNT];
	int64_t put_count;
	int64_t get_count;
	int64_t delete_count;
	// a read answers NULL, as a host that could not build its answer would
	int get_answers_null;
	// a read answers text that is no json
	int get_answers_garbage;
} forward_key_store;

static int forward_key_store_find(forward_key_store* store, const char* name) {
	for (int i = 0; i < FORWARD_KEY_STORE_ITEM_COUNT; i += 1) {
		if (store->used[i] && strcmp(store->names[i], name) == 0) {
			return i;
		}
	}
	return -1;
}

static int64_t forward_key_store_put_secret(void* user_data, const char* name, const char* value_base64) {
	forward_key_store* store = (forward_key_store*)user_data;
	__atomic_add_fetch(&store->put_count, 1, __ATOMIC_SEQ_CST);
	if (name == NULL || value_base64 == NULL || strlen(name) >= FORWARD_KEY_STORE_NAME_BYTE_COUNT || strlen(value_base64) >= FORWARD_KEY_STORE_VALUE_BYTE_COUNT) {
		return 0;
	}
	int at = forward_key_store_find(store, name);
	for (int i = 0; at < 0 && i < FORWARD_KEY_STORE_ITEM_COUNT; i += 1) {
		if (!store->used[i]) {
			at = i;
		}
	}
	if (at < 0) {
		return 0;
	}
	strcpy(store->names[at], name);
	strcpy(store->values[at], value_base64);
	store->used[at] = 1;
	return 1;
}

// The answer is malloc'd, as urnet_copy_string's is in a host that links the library: the library frees it.
static char* forward_key_store_get_secret(void* user_data, const char* name) {
	forward_key_store* store = (forward_key_store*)user_data;
	__atomic_add_fetch(&store->get_count, 1, __ATOMIC_SEQ_CST);
	if (store->get_answers_null) {
		return NULL;
	}
	if (store->get_answers_garbage) {
		return strdup("not json");
	}
	int at = name == NULL ? -1 : forward_key_store_find(store, name);
	if (at < 0) {
		return strdup("{\"status\":3}");
	}
	const char* head = "{\"status\":2,\"value_base64\":\"";
	const char* tail = "\"}";
	char* answer = (char*)malloc(strlen(head) + strlen(store->values[at]) + strlen(tail) + 1);
	if (answer == NULL) {
		return NULL;
	}
	strcpy(answer, head);
	strcat(answer, store->values[at]);
	strcat(answer, tail);
	return answer;
}

static int64_t forward_key_store_delete_secret(void* user_data, const char* name) {
	forward_key_store* store = (forward_key_store*)user_data;
	__atomic_add_fetch(&store->delete_count, 1, __ATOMIC_SEQ_CST);
	int at = name == NULL ? -1 : forward_key_store_find(store, name);
	if (at >= 0) {
		store->used[at] = 0;
	}
	return 1;
}

static forward_key_store* forward_key_store_new(void) {
	return (forward_key_store*)calloc(1, sizeof(forward_key_store));
}

static void* forward_key_store_put_secret_pointer(void) { return (void*)forward_key_store_put_secret; }
static void* forward_key_store_get_secret_pointer(void) { return (void*)forward_key_store_get_secret; }
static void* forward_key_store_delete_secret_pointer(void) { return (void*)forward_key_store_delete_secret; }

static int64_t forward_key_store_count(int64_t* count) { return __atomic_load_n(count, __ATOMIC_SEQ_CST); }
*/
import "C"

import (
	"unsafe"
)

// A forwarding key store in C memory, with the three C functions an app passes for it. Its functions take one call
// at a time, which is all these tests make; the counts read from any goroutine.
type ForwardKeyStore struct {
	store *C.forward_key_store
}

// A store holding nothing. Free it once no device or adapter that was given it can call it.
func NewForwardKeyStore() *ForwardKeyStore {
	return &ForwardKeyStore{store: C.forward_key_store_new()}
}

// Releases the store's memory.
func (self *ForwardKeyStore) Free() {
	C.free(unsafe.Pointer(self.store))
	self.store = nil
}

// The user_data the three functions take: the store itself.
func (self *ForwardKeyStore) UserData() unsafe.Pointer {
	return unsafe.Pointer(self.store)
}

// The C function a host passes as put_secret.
func (self *ForwardKeyStore) PutSecretFunction() unsafe.Pointer {
	return C.forward_key_store_put_secret_pointer()
}

// The C function a host passes as get_secret.
func (self *ForwardKeyStore) GetSecretFunction() unsafe.Pointer {
	return C.forward_key_store_get_secret_pointer()
}

// The C function a host passes as delete_secret.
func (self *ForwardKeyStore) DeleteSecretFunction() unsafe.Pointer {
	return C.forward_key_store_delete_secret_pointer()
}

// How many calls put_secret, get_secret and delete_secret took.
func (self *ForwardKeyStore) CallCounts() (putCount int64, getCount int64, deleteCount int64) {
	return int64(C.forward_key_store_count(&self.store.put_count)),
		int64(C.forward_key_store_count(&self.store.get_count)),
		int64(C.forward_key_store_count(&self.store.delete_count))
}

// The value the store holds under name, as put_secret took it.
func (self *ForwardKeyStore) Value(name string) (string, bool) {
	nameC := C.CString(name)
	defer C.free(unsafe.Pointer(nameC))
	at := C.forward_key_store_find(self.store, nameC)
	if at < 0 {
		return "", false
	}
	return C.GoString(&self.store.values[at][0]), true
}

// Makes get_secret answer NULL from now on, or stop.
func (self *ForwardKeyStore) SetGetAnswersNull(answersNull bool) {
	self.store.get_answers_null = boolInt(answersNull)
}

// Makes get_secret answer text that is no json from now on, or stop.
func (self *ForwardKeyStore) SetGetAnswersGarbage(answersGarbage bool) {
	self.store.get_answers_garbage = boolInt(answersGarbage)
}

func boolInt(value bool) C.int {
	if value {
		return 1
	}
	return 0
}
