package main

/*
#include <stdlib.h>
#include <stdint.h>
*/
import "C"

import (
	"encoding/json"
	"unsafe"

	"github.com/urnetwork/glog/v2026"
)

// strings returned to the C side are malloc'd copies that the caller
// must free with urnet_free_string

func cString(s string) *C.char {
	return C.CString(s)
}

func cStringFree(p *C.char) {
	C.free(unsafe.Pointer(p))
}

func goString(p *C.char) string {
	if p == nil {
		return ""
	}
	return C.GoString(p)
}

func cJson(value any, name string) *C.char {
	if value == nil {
		return nil
	}
	b, err := json.Marshal(value)
	if err != nil {
		glog.Errorf("[cgo]%s: marshal: %v", name, err)
		return nil
	}
	return C.CString(string(b))
}

// goJson parses a json argument into out. A nil/empty input leaves out at its zero value.
func goJson(p *C.char, out any, name string) bool {
	if p == nil {
		return true
	}
	s := C.GoString(p)
	if s == "" {
		return true
	}
	if err := json.Unmarshal([]byte(s), out); err != nil {
		glog.Errorf("[cgo]%s: unmarshal: %v", name, err)
		return false
	}
	return true
}

func setErrorOut(outError **C.char, err error) {
	if outError == nil || err == nil {
		return
	}
	*outError = C.CString(err.Error())
}

func goBytes(p *C.uint8_t, n C.int32_t) []byte {
	if p == nil || n <= 0 {
		return nil
	}
	return C.GoBytes(unsafe.Pointer(p), C.int(n))
}
