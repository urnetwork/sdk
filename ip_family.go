package sdk

import "github.com/urnetwork/connect"

// ip_family.go — the provider address-family category as the apps see it: on
// every provider dot (ProviderGridPoint), every connected-provider row
// (ConnectedProviderLocation) and the window status counts. See IPV6.md D1.
//
// Two plain-string forms are exported because gomobile cannot bind a named
// string type from another package. The category is the stable machine value
// the apps switch on; the label is the short text the histogram rows and the
// provider rows print. A connect legacy (empty) category is normalized to
// v4-only at this boundary, which is what a legacy provider carries, so the
// apps never see an empty value.

// The provider address-family categories, as `ProviderGridPoint.IpFamily` and
// `ConnectedProviderLocation.IpFamily` carry them.
const (
	IpFamilyDualstack = "dualstack"
	IpFamilyV4Only    = "v4-only"
	IpFamilyV6Only    = "v6-only"
)

// The short display labels, as `ProviderGridPoint.IpFamilyLabel` and
// `ConnectedProviderLocation.IpFamilyLabel` carry them. The histogram rows in
// the connect drawer are titled with these.
const (
	IpFamilyLabelBoth = "both"
	IpFamilyLabelV4   = "v4"
	IpFamilyLabelV6   = "v6"
)

// ipFamilyValue normalizes a connect category at the sdk boundary: legacy and
// unknown read as v4-only.
func ipFamilyValue(family connect.IpFamily) string {
	switch family.Normalize() {
	case connect.IpFamilyDualstack:
		return IpFamilyDualstack
	case connect.IpFamilyV6Only:
		return IpFamilyV6Only
	default:
		return IpFamilyV4Only
	}
}

// ipFamilyLabel is the display label for a connect category.
func ipFamilyLabel(family connect.IpFamily) string {
	switch family.Normalize() {
	case connect.IpFamilyDualstack:
		return IpFamilyLabelBoth
	case connect.IpFamilyV6Only:
		return IpFamilyLabelV6
	default:
		return IpFamilyLabelV4
	}
}
