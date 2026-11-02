package sdk

import (
	"context"
	"sync/atomic"

	"github.com/urnetwork/connect"
)

// extender_camouflage_report.go — the network space's side of the camouflage report
// (connect/net_extender_camouflage_report.go; connect/EXTENDER-SPLICE-ROLLOUT.md,
// Reporting path). connect has the sender, which a connectctl extender runs too.
//
// The space runs one sender over the identity bound to its gated release fetch, the
// client token of the device that dials through the space. It posts over the space's
// api transport, so a client whose control route crosses an extender can still say
// how its dials went. An account change, sign-out or unbind, seen through the space's
// account monitor, cancels what is in flight and drops everything counted so far. A
// hosted space runs none, since it is shared by unrelated customers, and a process
// whose extender store is read-only binds no identity, so it sends nothing.
//
// The cohort control is the gated preference's, NetworkSpaceValues.ExtenderGatedPreference,
// off by default and applied in place: every cohort enabled for gated selection
// reports what its camouflage dials came to from the first hour, nothing outside a
// cohort is sent, and no new value reaches the exported surface. The operator's own
// switch is `camouflage_report` in `extender.yml`.
//
// A device's extender role adds its server as the sender's source while it runs
// (device_local_extender_native.go), so the role's own numbers and verdict go in the
// same report under the same identity.

// The sender of one space, nil for a space that runs no release fetch: no directory,
// or hosted.
type extenderCamouflageReportSpace struct {
	reporter atomic.Pointer[connect.ExtenderCamouflageReporter]
}

// Test seam: adjusts the settings of every sender built after it is set.
var extenderCamouflageReportConfigure func(settings *connect.ExtenderCamouflageReporterSettings)

// Builds the space's sender, after its release fetch, whose identity it reports
// under. It starts in the state of the cohort control.
func (self *NetworkSpace) startExtenderCamouflageReport() {
	fetcher := self.extenderRelease.fetcher.Load()
	if fetcher == nil {
		return
	}
	settings := connect.DefaultExtenderCamouflageReporterSettings()
	settings.Log = self.logger()
	settings.ApiUrl = self.apiUrl
	settings.ByJwt = fetcher.boundByJwt
	api := self.api
	settings.Post = func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
		return api.transportHttpPostRaw()(ctx, requestUrl, requestBodyBytes, byJwt)
	}
	settings.AccountMonitor = self.extenderRelease.accountMonitor()
	settings.ClientVersion = func() string { return Version }
	directory := self.extenderDirectory
	settings.ResourceVersion = func() int {
		borrowList := connect.ExtenderCamouflageBorrowListForCountry(directory.SpoofCountryCode())
		if borrowList == nil {
			return 0
		}
		return borrowList.Version
	}
	if extenderCamouflageReportConfigure != nil {
		extenderCamouflageReportConfigure(settings)
	}
	reporter := connect.NewExtenderCamouflageReporter(self.ctx, settings)
	self.extenderCamouflageReport.reporter.Store(reporter)
	reporter.SetEnabled(self.valuesCopy().ExtenderGatedPreference)
}

// Stops the sender; it posts through the strategy the space closes after it.
func (self *NetworkSpace) closeExtenderCamouflageReport() {
	if reporter := self.extenderCamouflageReport.reporter.Load(); reporter != nil {
		reporter.Close()
	}
}

// Turns the sender on or off with the cohort control.
func (self *NetworkSpace) applyExtenderCamouflageReport(enabled bool) {
	if reporter := self.extenderCamouflageReport.reporter.Load(); reporter != nil {
		reporter.SetEnabled(enabled)
	}
}

// Adds a device's extender role to what the sender reports.
func (self *NetworkSpace) setExtenderCamouflageReportSource(source connect.ExtenderCamouflageReportSource) {
	if reporter := self.extenderCamouflageReport.reporter.Load(); reporter != nil {
		reporter.SetSource(source)
	}
}

// Takes a closing role out of what the sender reports, if it is still this one.
func (self *NetworkSpace) clearExtenderCamouflageReportSource(source connect.ExtenderCamouflageReportSource) {
	if reporter := self.extenderCamouflageReport.reporter.Load(); reporter != nil {
		reporter.ClearSource(source)
	}
}

// The sender's counts, empty for a space that runs none.
func (self *NetworkSpace) extenderCamouflageReportStats() connect.ExtenderCamouflageReporterStats {
	if reporter := self.extenderCamouflageReport.reporter.Load(); reporter != nil {
		return reporter.Stats()
	}
	return connect.ExtenderCamouflageReporterStats{}
}

// The client token of the bound identity, empty when none is bound or the token in
// hand is another identity's, which is an account change the binding has not seen
// yet. The sender reads it at each look and each post, so nothing goes out under an
// identity the account monitor has not announced.
func (self *extenderReleaseFetcher) boundByJwt() string {
	credential, identity := self.boundCredential()
	if credential == nil || identity.empty() {
		return ""
	}
	byJwt := credential()
	if parseExtenderReleaseIdentity(byJwt) != identity {
		return ""
	}
	return byJwt
}
