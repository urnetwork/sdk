// The forwarding role's clock for the apps (sidecar §17.8's after sleep or a clock step, owner decision 789, QD-1404):
// each role's device.Bridge acts on its rules' wall-clock times, a map's or reach row's expires, a drain's end and the
// previous block's drain, by waiting on runtime timers, which stop while the device sleeps, so a phone that slept past
// one would keep that rule's earlier flows open for the timer's remaining awake time. DeviceLocal.CheckClock, which
// the VPN component calls when the device wakes and when its clock changes, has every role's bridge publish at now,
// ending what lapsed, and arm its waits again. The bridges on iOS and Android take no cap on those waits and add no
// periodic wakeup (device.MobileBridgeSettings), so there these calls keep an expiry's reset at once; every other
// device's, a low-memory one's included, also caps each wait at a desktop's ClockCheckInterval
// (deviceLocalBridgeSettingsFor), so a desktop app that never calls it is late by at most that after it wakes.
// Untagged, as device_sidecar.go is, since the tunnel links it.
//
// The call sites, which the app phase writes, since the apps' code is not in these repos; the call returns at once
// and never blocks, so each may make it on whatever thread the platform calls it on:
//
//	Apple     the network extension's NEPacketTunnelProvider subclass, on iOS and macOS: override wake(), which the
//	          system calls once the device has woken, and call the extension's device's checkClock() there
//	          (SdkDeviceLocal); and observe Foundation's NSSystemClockDidChange notification, posted when the clock is
//	          set, calling it again. sleep(completionHandler:) needs nothing.
//	Android   the VPN service, while its device runs, registers one receiver at runtime (ContextCompat.registerReceiver,
//	          RECEIVER_NOT_EXPORTED, since each is a system broadcast) for Intent.ACTION_TIME_CHANGED, the clock set,
//	          Intent.ACTION_SCREEN_ON, the device in use again, and PowerManager.ACTION_DEVICE_IDLE_MODE_CHANGED, calling
//	          the device's checkClock() at each but an idle-mode change into idle (PowerManager.isDeviceIdleMode()
//	          true), so doze's exit counts; it unregisters as the device closes. ACTION_SCREEN_ON is not delivered to a
//	          receiver the manifest declares.
//	Windows,  the app services' bridges cap their waits (a desktop's settings); a service that wants the reset at once
//	Linux     calls urnet_device_local_check_clock at the power-resume notification (WM_POWERBROADCAST's or
//	          SERVICE_CONTROL_POWEREVENT's PBT_APMRESUMEAUTOMATIC) and at logind's PrepareForSleep(false), as ur daemon
//	          does (sidecar/cli/resume_windows.go, resume_linux.go).

package sdk

// Has every forwarding role's bridge read the clock again at once (the file's comment): each publishes at now, ending
// the flows of an expiry or a drain's end that the device slept past or a step of its clock passed, and arms its waits
// again from now. The VPN component calls it when the device wakes and when its clock changes. Returns at once, from
// any thread: it reads which bridge the device holds and hands each a check, waiting on none. A device that holds
// none, in legacy, off, hosted or closed, does nothing, since nothing of its rules is timed.
func (self *DeviceLocal) CheckClock() {
	trace(func() {
		held := func() deviceBridge {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return self.bridge
		}()
		if held != nil {
			held.CheckClock()
		}
	})
}

// deviceBridge's CheckClock over several roles: each role's bridge reads the clock again, the primary's among them.
func (self *deviceBridgeRouter) CheckClock() {
	for _, role := range self.roles {
		role.bridge.CheckClock()
	}
}
