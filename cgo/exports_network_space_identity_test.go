// Native binding handles must compare the underlying owner, not handle numbers.
package main

import (
	"reflect"
	"testing"

	"github.com/urnetwork/sdk/v2026"
)

// The actual generated export resolves separate handles before pointer identity.
func TestNetworkSpaceSameExportComparesNativeOwners(t *testing.T) {
	space := &sdk.NetworkSpace{}
	first := newHandle(space)
	alias := newHandle(space)
	replacement := newHandle(&sdk.NetworkSpace{})
	wrongType := newHandle("synthetic-wrong-handle-type")
	defer handleRelease(first)
	defer handleRelease(alias)
	defer handleRelease(replacement)
	defer handleRelease(wrongType)

	// Reflection converts the Go test's handles to the generator's C typedef;
	// importing C in a _test.go file is unsupported by the Go toolchain.
	export := reflect.ValueOf(urnet_network_space_same)
	for _, test := range []struct {
		name  string
		self  uint64
		other uint64
		want  bool
	}{
		{"same handle", first, first, true},
		{"separate handles of one native object", first, alias, true},
		{"equal-key different native object", first, replacement, false},
		{"absent argument", first, 0, false},
		{"absent receiver", 0, first, false},
		{"unknown argument", first, uint64(1) << 62, false},
		{"wrong-type argument", first, wrongType, false},
	} {
		values := export.Call([]reflect.Value{
			reflect.ValueOf(test.self).Convert(export.Type().In(0)),
			reflect.ValueOf(test.other).Convert(export.Type().In(1)),
		})
		if values[0].Bool() != test.want {
			t.Errorf("%s: native identity was %v, want %v", test.name, values[0].Bool(), test.want)
		}
	}
}
