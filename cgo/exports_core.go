package main

/*
#include <stdlib.h>
#include <stdint.h>
#include <stdbool.h>
*/
import "C"

import (
	"unsafe"

	"github.com/urnetwork/sdk/v2026"
)

// core exports that are part of the abi contract rather than the sdk surface.
// the sdk surface exports are generated, see gen/gen.go

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
