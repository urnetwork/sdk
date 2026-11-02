//go:build !ios && !android && !js

package sdk

import (
	"testing"

	"github.com/urnetwork/connect"
)

// A device's extender role adds its server to what the space reports while it runs,
// and takes it out when it closes (extender_camouflage_report.go), so a role's numbers
// go in the one report of the space, under the device's identity. Another role's
// server is left alone.
func TestExtenderCamouflageReportHoldsTheRolesServerWhileItRuns(t *testing.T) {
	newTestCamouflageSpaceSeams(t)
	fixture := newTestProvideExtenderFixture(t, nil)
	role := fixture.extender()
	reporter := fixture.networkSpace.extenderCamouflageReport.reporter.Load()
	if reporter == nil {
		t.Fatal("the space runs no sender")
	}
	server := role.currentServer()
	if server == nil {
		t.Fatal("the role has no server")
	}
	if reporter.Source() != connect.ExtenderCamouflageReportSource(server) {
		t.Fatalf("source = %v", reporter.Source())
	}
	// what the role hands over reads as a verdict of camouflage off, today's production
	counts := server.TakeCamouflageReportCounts()
	if counts.Covered || counts.DegradedReason == "" {
		t.Errorf("counts = %+v", counts)
	}

	role.Close()
	if source := reporter.Source(); source != nil {
		t.Fatalf("a closed role's server stayed: %v", source)
	}
}
