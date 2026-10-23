//go:build js

// The provider rows on ur.io draw the extenders a connected provider carries
// (EXTENDER.md K9): the binding pairs the sdk's comma-separated addresses and
// colors into an array of {ip, colorHex}.
package main

import (
	"syscall/js"
	"testing"

	"github.com/urnetwork/sdk"
)

func TestConnectedProviderLocationCarriesItsExtenders(t *testing.T) {
	extenders := jsConnectedProviderLocation(&sdk.ConnectedProviderLocation{
		ExtenderIps:        "203.0.113.7,2001:db8::5",
		ExtenderColorHexes: "AABBCC,DDEEFF",
	}).Get("extenders")
	if extenders.Length() != 2 {
		t.Fatalf("extenders = %d, want 2", extenders.Length())
	}
	for i, want := range [][2]string{{"203.0.113.7", "AABBCC"}, {"2001:db8::5", "DDEEFF"}} {
		extender := extenders.Index(i)
		if extender.Get("ip").String() != want[0] || extender.Get("colorHex").String() != want[1] {
			t.Fatalf("extender %d = %s %s, want %s %s", i,
				extender.Get("ip").String(), extender.Get("colorHex").String(), want[0], want[1])
		}
	}
}

// A direct or p2p provider has an empty array, never a missing field.
func TestDirectProviderLocationHasNoExtenders(t *testing.T) {
	extenders := jsConnectedProviderLocation(&sdk.ConnectedProviderLocation{}).Get("extenders")
	if extenders.Type() != js.TypeObject || extenders.Length() != 0 {
		t.Fatalf("a direct provider's extenders = %v", extenders)
	}
}

// The sdk keeps the two lists the same length; an unpaired tail is dropped
// rather than drawn in a made-up color.
func TestProviderExtendersDropAnUnpairedTail(t *testing.T) {
	extenders := jsProviderExtenders("203.0.113.7,198.51.100.2", "AABBCC")
	if len(extenders) != 1 {
		t.Fatalf("extenders = %d, want 1", len(extenders))
	}
}
