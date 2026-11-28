package sdk

// What a release request names for the tcp camouflage (EXTENDER-UPDATE2 §3.2,
// contract R). The decision is connect's: a client claims tcp-camouflage-v1 only
// with the Chrome hello and a measured, current borrow list for its country, so
// it is never issued a splicing extender's record it could not dial.

import (
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// A measured, current list of one name, for the country given ("" is global).
func testReleaseBorrowList(countryCode string) *connect.ExtenderBorrowList {
	return &connect.ExtenderBorrowList{
		CountryCode:       countryCode,
		Version:           1,
		Measured:          true,
		MeasuredAt:        time.Now(),
		MaxMeasurementAge: 24 * time.Hour,
		Names:             []connect.ExtenderBorrowName{{Name: "borrow.example"}},
	}
}

// Installs the lists for the rest of the test.
func installTestReleaseBorrowLists(t *testing.T, borrowLists ...*connect.ExtenderBorrowList) {
	t.Helper()
	t.Cleanup(connect.Testing_SetExtenderCamouflageBorrowLists(borrowLists))
}

func TestExtenderReleaseCapabilitiesAreConnectsDecision(t *testing.T) {
	tcpCamouflage := []string{connect.ExtenderCapabilityTcpCamouflageV1}
	connectSettings := connect.DefaultConnectSettings()

	// a list with no selectable name, as the bundled placeholder is
	noNames := testReleaseBorrowList("")
	noNames.Names = nil
	installTestReleaseBorrowLists(t, noNames)
	if capabilities := extenderReleaseCapabilities(connectSettings, ""); len(capabilities) != 0 {
		t.Fatalf("with no selectable name the client claimed %v", capabilities)
	}

	installTestReleaseBorrowLists(t, testReleaseBorrowList(""))
	if capabilities := extenderReleaseCapabilities(connectSettings, ""); !slices.Equal(capabilities, tcpCamouflage) {
		t.Fatalf("with a measured list the client claimed %v", capabilities)
	}

	// the country reaches the decision: a country's own stale list is not
	// replaced by the global one, so its clients claim nothing
	stale := testReleaseBorrowList("ir")
	stale.MeasuredAt = time.Now().Add(-48 * time.Hour)
	installTestReleaseBorrowLists(t, testReleaseBorrowList(""), stale)
	if capabilities := extenderReleaseCapabilities(connectSettings, "ir"); len(capabilities) != 0 {
		t.Fatalf("with its country's stale list the client claimed %v", capabilities)
	}
	if capabilities := extenderReleaseCapabilities(connectSettings, "de"); !slices.Equal(capabilities, tcpCamouflage) {
		t.Fatalf("another country on the global list claimed %v", capabilities)
	}

	// the go fingerprint sends no authenticated hello
	goSettings := connect.DefaultConnectSettings()
	goSettings.TlsClientHelloFingerprint = connect.TlsClientHelloFingerprintGo
	if capabilities := extenderReleaseCapabilities(goSettings, "de"); len(capabilities) != 0 {
		t.Fatalf("with the go fingerprint the client claimed %v", capabilities)
	}
}

// The request carries the decision.
func TestExtenderReleaseRequestNamesTheClientCapabilities(t *testing.T) {
	installTestReleaseBorrowLists(t, testReleaseBorrowList(""))
	connectSettings := connect.DefaultConnectSettings()
	harness := newTestReleaseHarness(t, func(harness *testReleaseHarness) {
		harness.settings.Capabilities = func() []string {
			return extenderReleaseCapabilities(connectSettings, "")
		}
	})
	harness.setByJwt(testReleaseByJwt(t, connect.NewId(), connect.NewId()))
	harness.fetcher.bind(harness.credential)

	post := harness.nextPost()
	if !slices.Equal(post.args.Capabilities, []string{connect.ExtenderCapabilityTcpCamouflageV1}) {
		t.Fatalf("the request named %v", post.args.Capabilities)
	}
	post.answer(testReleaseAnswer("empty", 0))
	harness.requireParked("after the answer")
}
