package sdk

import (
	"testing"

	"github.com/urnetwork/connect"
)

// the sdk boundary normalizes every connect category to one of the three
// exported values and labels; legacy and unknown read as v4-only, which is
// what they carry (IPV6.md D1)
func TestIpFamilyValueAndLabel(t *testing.T) {
	cases := []struct {
		family connect.IpFamily
		value  string
		label  string
	}{
		{connect.IpFamilyDualstack, IpFamilyDualstack, IpFamilyLabelBoth},
		{connect.IpFamilyV4Only, IpFamilyV4Only, IpFamilyLabelV4},
		{connect.IpFamilyV6Only, IpFamilyV6Only, IpFamilyLabelV6},
		{connect.IpFamilyLegacy, IpFamilyV4Only, IpFamilyLabelV4},
		{connect.IpFamily("something-newer"), IpFamilyV4Only, IpFamilyLabelV4},
	}
	for _, c := range cases {
		connect.AssertEqual(t, ipFamilyValue(c.family), c.value)
		connect.AssertEqual(t, ipFamilyLabel(c.family), c.label)
	}
	// the exported values are the connect wire values, so an app can compare
	// them against what find-providers2 reports
	connect.AssertEqual(t, IpFamilyDualstack, string(connect.IpFamilyDualstack))
	connect.AssertEqual(t, IpFamilyV4Only, string(connect.IpFamilyV4Only))
	connect.AssertEqual(t, IpFamilyV6Only, string(connect.IpFamilyV6Only))
}
