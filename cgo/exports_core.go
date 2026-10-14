package main

/*
#include <stdlib.h>
#include <stdint.h>
#include <stdbool.h>
*/
import "C"

import (
	"unsafe"

	"github.com/urnetwork/sdk"
)

// core exports that are part of the abi contract rather than the sdk surface.
// the sdk surface exports are generated, see gen/gen.go

// urnet_abi_version changes only for an incompatible C ABI revision. Additive
// exports do not require a bump; every language binding checks this on load.
// 2: the three generated messaging exports left this library when messaging
// moved to github.com/urnetwork/message.
//
//export urnet_abi_version
func urnet_abi_version() C.int32_t { return 2 }

//export urnet_version
func urnet_version() *C.char {
	defer cgoGuard("urnet_version")
	return cString(sdk.Version)
}

//export urnet_free_string
func urnet_free_string(p *C.char) {
	if p != nil {
		C.free(unsafe.Pointer(p))
	}
}

// urnet_copy_string returns a copy of s allocated by this library, or NULL for
// NULL. A callback that returns char* must return a string from here: the sdk
// frees the result with urnet_free_string, which is this library's free, and a
// buffer the host allocated itself would be freed on a heap that never
// allocated it whenever the host links a different C runtime (windows/
// urnetwork#17: a service crash in RtlFreeHeap).
//
//export urnet_copy_string
func urnet_copy_string(s *C.char) *C.char {
	if s == nil {
		return nil
	}
	return cString(goString(s))
}

//export urnet_release
func urnet_release(handle C.uint64_t) C.bool {
	defer cgoGuard("urnet_release")
	return C.bool(handleRelease(uint64(handle)))
}

//export urnet_live_handle_count
func urnet_live_handle_count() C.int64_t {
	defer cgoGuard("urnet_live_handle_count")
	return C.int64_t(handleCount())
}

func main() {
	// required for buildmode=c-shared. never called.
}
