//go:build !sdk_mobile_bind

// Go native packet readers use the same ready-only remote admission as the
// existing packed and descriptor adapters. The binding view excludes pooled
// packet ownership; portable callers keep their packed byte entry point.
package sdk

// Takes every pooled packet on entry and borrows the outer slice. Remote
// routes make one ready offer; local protocol owners and custom routes keep
// their existing bounded timeout. The count is admissions, not a prefix.
//
//gomobile:noexport
func (self *DeviceLocal) SendNativePacketsNoCopy(packets [][]byte) int {
	return self.sendPacketsNoCopy(packets)
}
