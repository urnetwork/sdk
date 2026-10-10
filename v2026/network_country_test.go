// The mobile network's country, which the extender dials fall back to for
// their spoof list (sdk.go SetNetworkCountryCode).
package sdk

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// The network country (open bug P052). An app reports the country of the
// mobile network the device is on, and the extender dials of every space fall
// back to it for their spoof list while the operator's hint cannot be had. It
// is applied in place: a space that is already running takes it from its next
// extender dial, with nothing rebuilt, and a space built afterwards starts
// with it. The test binary runs no extender network client, so no operator
// hint competes with the report here.
func TestSetNetworkCountryCodeAppliesInPlace(t *testing.T) {
	previousCountryCode := connect.NetworkCountryCode()
	t.Cleanup(func() {
		connect.SetNetworkCountryCode(previousCountryCode)
	})
	SetNetworkCountryCode("")

	ctx := context.Background()
	networkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("space.example", "main"),
		NetworkSpaceValues{},
		"",
	)
	defer networkSpace.close()
	directory := networkSpace.extenderDirectory
	if directory == nil {
		t.Fatal("the space has no extender directory")
	}
	connect.AssertEqual(t, directory.SpoofCountryCode(), "")

	// on cellular in Russia: the running space's directory has it at once
	SetNetworkCountryCode("RU")
	connect.AssertEqual(t, directory.SpoofCountryCode(), "ru")

	// a space built after the report starts with it, which is why an app
	// reports before the network space manager builds its spaces
	laterNetworkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("other.example", "main"),
		NetworkSpaceValues{},
		"",
	)
	defer laterNetworkSpace.close()
	connect.AssertEqual(t, laterNetworkSpace.extenderDirectory.SpoofCountryCode(), "ru")

	// on Wi-Fi the app clears it, in place as well
	SetNetworkCountryCode("")
	connect.AssertEqual(t, directory.SpoofCountryCode(), "")
	connect.AssertEqual(t, laterNetworkSpace.extenderDirectory.SpoofCountryCode(), "")

	// what is not a country clears it rather than naming a list
	SetNetworkCountryCode("kz")
	connect.AssertEqual(t, directory.SpoofCountryCode(), "kz")
	SetNetworkCountryCode("not a country")
	connect.AssertEqual(t, directory.SpoofCountryCode(), "")
}
