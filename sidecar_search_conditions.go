//go:build !ios_extension && !js

// The host's conditions and foreground, as an app reports them to its search (§18.7's Conditions, foreground row,
// D-SDK-23, plan-SEARCH-foreground-signal; SDK-M10-02):
//
//	conditions  SetConditions(unmetered, charging, lowPower) on each change: iOS NWPathMonitor (unmetered = neither
//	            isExpensive nor isConstrained), UIDevice batteryState, ProcessInfo isLowPowerModeEnabled; Android
//	            NET_CAPABILITY_NOT_METERED, BatteryManager.isCharging, PowerManager.isPowerSaveMode. A condition not
//	            yet reported counts as metered and not charging, so a phone fetches nothing large before it says
//	            otherwise
//	foreground  SetForeground at start and on each lifecycle change; an unreported state counts as background. The
//	            VPN component's running never counts as the foreground (owner decision 197)
//
// The Go core reads no platform API: these reports are all it knows. A remote-mode Sidecar's daemon reports none
// (§18.7), so on that backend they go nowhere. Safe for concurrent use.

package sdk

// Reports the host's conditions (§18.5): the app calls it on each change.
func (self *SidecarSearch) SetConditions(unmetered bool, charging bool, lowPower bool) {
	trace(func() {
		if self.ctx.Err() != nil {
			return
		}
		self.backend.setConditions(unmetered, charging, lowPower)
	})
}

// Reports whether the app is in the foreground (§18.5): the app calls it at start and on each lifecycle change, a page
// Worker's frame on tab visibility. It feeds the core's foreground (Sidecar.SetSearchForeground, SEARCH-M10-33's) and
// this host's class rule (sidecarSearchHost.mayRun).
func (self *SidecarSearch) SetForeground(foreground bool) {
	trace(func() {
		if self.host != nil {
			self.host.setForeground(foreground)
		}
		if self.ctx.Err() != nil {
			return
		}
		self.backend.setForeground(foreground)
	})
}
