package sdk

import "testing"

func TestFips140Disabled(t *testing.T) {
	if GetFips140Enabled() {
		t.Fatal("FIPS 140 mode is outside the iOS network-extension memory budget")
	}
}
