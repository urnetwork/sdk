//go:build !ios_extension && !js

// The app's sidecar role reads its clock again after a sleep or a clock step (§13.17's app listeners, §17.8's core's
// own ends, §18.5's API list; owner decision 803, QD-1427). An app's tls/ and smtp/ listens run on the core's sockets
// component while the app is open (sidecar_sockets.go, sidecar_tls.go), which resets a stream whose ticket names a
// grant_expire_time_millis with RST_REVOKED at that end (§9.6's step 7). Its wait is a runtime timer, which stops while
// the device sleeps and does not move when the clock is set, so after the phone sleeps, the OS suspends the app and
// resumes it, or the clock steps forward, such a stream would stay open past its grant's end for the timer's remaining
// awake time. Sidecar.CheckClock hands the core's Sidecar.CheckClock the check (sidecar's core_clock_check.go): the
// sockets component arms each such wait again from the clock's now, the end fixed at admission, and resets at once
// each stream whose end has passed. A new stream needs no check, since its ticket's expiry refuses it at admission by
// the wall clock, and revocations act at once without one.
//
// The VPN component's forwarding role takes its own call, DeviceLocal.CheckClock (sidecar_clock_check.go): another
// object, and on iOS another process, so an app that runs both makes both calls at the same moments. On the remote
// backend, which a desktop app runs, the sidecar role is ur daemon's, which checks its own clock at each resume and
// every ClockCheckInterval (sidecar's cli/resume.go), so the call does nothing there, nor once the Sidecar has closed.
//
// The call sites, which the app phase writes, since the apps' code is not in these repos. It is a host call the app
// makes, not a view controller, and it returns at once without blocking, so each may make it on whatever thread the
// platform calls it on; calling it more often costs only the re-arming:
//
//	iOS       the app, when it comes to the foreground (UIApplication.willEnterForegroundNotification, or a scene's
//	          scenePhase becoming active), and at Foundation's NSSystemClockDidChange notification, posted when the
//	          clock is set, calling its Sidecar's checkClock() (SdkSidecar); the network extension calls its device's
//	          checkClock() at its own wake()
//	Android   the app, when it comes to the foreground (ProcessLifecycleOwner's ON_START), and, while its Sidecar is
//	          open, at Intent.ACTION_TIME_CHANGED, the clock set, through a receiver it registers at runtime
//	          (ContextCompat.registerReceiver, RECEIVER_NOT_EXPORTED, since it is a system broadcast) and unregisters as
//	          the Sidecar closes, calling the Sidecar's checkClock(); the VPN service calls its device's checkClock() at
//	          its own broadcasts
//	desktops  nothing: the Windows, Linux and macOS apps reach ur daemon in remote mode (§5.8); urnet_sidecar_check_clock
//	          is the C ABI's for a host that runs the local backend
//
// Tagged as sidecar.go is: the iOS extension links no sdk.Sidecar, and js's Sidecar is the remote one alone.

package sdk

import (
	"github.com/urnetwork/sidecar"
)

// Has the core read the clock again at once (the file's comment): its sockets component arms the wait for each
// listener's streams' grant ends anew from now and resets with RST_REVOKED each whose end the device slept past or a
// step of its clock passed. Returns at once, from any thread: it reads which core this Sidecar holds and hands it the
// check, which waits on no stream and makes no call. The remote backend's daemon checks its own clock, so there, as
// once the Sidecar has closed, it does nothing.
func (self *Sidecar) CheckClock() {
	trace(func() {
		local := func() *sidecar.Sidecar {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.closed {
				return nil
			}
			return self.local
		}()
		if local != nil {
			local.CheckClock()
		}
	})
}
