package main

/*
#include <stdint.h>
*/
import "C"

import (
	"github.com/urnetwork/glog"
	"github.com/urnetwork/sdk"
)

// conversions between the c abi forms and the sdk forms:
// - `sdk.Id` crosses as a uuid string
// - `sdk.Time` crosses as unix epoch milliseconds, where 0 means no time

func goId(p *C.char, name string) *sdk.Id {
	if p == nil {
		return nil
	}
	s := goString(p)
	if s == "" {
		return nil
	}
	id, err := sdk.ParseId(s)
	if err != nil {
		glog.Errorf("[cgo]%s: invalid id %q: %v", name, s, err)
		return nil
	}
	return id
}

func cId(id *sdk.Id) *C.char {
	if id == nil {
		return nil
	}
	return cString(id.String())
}

func goTime(unixMilli C.int64_t) *sdk.Time {
	if unixMilli == 0 {
		return nil
	}
	return sdk.NewTimeUnixMilli(int64(unixMilli))
}

func cTime(t *sdk.Time) C.int64_t {
	if t == nil {
		return 0
	}
	return C.int64_t(t.UnixMilli())
}
