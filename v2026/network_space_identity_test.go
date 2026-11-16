// Native callback admission needs object identity, including equal-key rebuilds.
package sdk

import "testing"

// A saved wrapper may refer to the same native object without being the same
// language wrapper. Comparing native pointers does not construct any resources.
func TestNetworkSpaceSameRetainsNativeOwner(t *testing.T) {
	space := &NetworkSpace{}
	alias := space
	if !space.Same(alias) || !alias.Same(space) {
		t.Fatal("the same native space lost identity")
	}
}

// Default and equal-key replacement objects are distinct lifecycle owners.
func TestNetworkSpaceSameRejectsReplacementWithEqualKey(t *testing.T) {
	space := &NetworkSpace{}
	replacement := &NetworkSpace{key: space.key, values: space.values}
	if space.Same(replacement) || replacement.Same(space) {
		t.Fatal("equal configuration admitted a replacement native owner")
	}
}

// The bridge can supply a nil argument; it is never a live space's owner.
func TestNetworkSpaceSameHandlesAbsentOwner(t *testing.T) {
	space := &NetworkSpace{}
	var absent *NetworkSpace
	if space.Same(absent) || absent.Same(space) || !absent.Same(nil) {
		t.Fatal("absent native identity changed pointer comparison semantics")
	}
}
