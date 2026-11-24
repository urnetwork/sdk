//go:build !ios_extension && !js

// The support skill in the apps (DESIGN-COMPUTE §28): the app process's support log ring (§28.7), opened beside the
// local core under the sidecar's state directory, which the core writes through and which records the app's sidecar
// starting and stopping. The app's support conversation, Support() (§15.3, §28.4), arrives with the support client.
//
// The ring is the app process's own, as ur daemon's is the daemon's: the VPN component, which runs only forwarding
// (owner decision 197), keeps none, and a remote Sidecar writes through its daemon's. One that cannot open leaves the
// app without one: the support log is never a reason to stop work (§28.14).

package sdk

import (
	"path/filepath"
	"time"

	"github.com/urnetwork/sidecar"
)

// A local Sidecar's ring and when it opened.
type sidecarSupportLog struct {
	log     *sidecar.SupportLog
	clock   sidecar.Clock
	started time.Time
}

// The ring under stateDir, set as coreSettings' support log, with the app's start recorded; nil where stateDir is no
// absolute directory or the ring cannot open.
func openSidecarSupportLog(stateDir string, coreSettings *sidecar.SidecarSettings) *sidecarSupportLog {
	if stateDir == "" || !filepath.IsAbs(stateDir) {
		return nil
	}
	clock := coreSettings.ClockOrReal()
	log, err := sidecar.OpenSupportLog(&sidecar.SupportLogArgs{
		Directory: sidecar.SupportLogDirectory(stateDir),
		Settings:  coreSettings.Support,
		Clock:     clock,
		Log:       coreSettings.Log,
	})
	if err != nil {
		return nil
	}
	coreSettings.SupportLog = log
	log.Event(sidecar.SupportLogCodeAppStarted, sidecar.SupportLogFieldRelease(Version))
	return &sidecarSupportLog{log: log, clock: clock, started: clock.Now()}
}

// Records the app's sidecar stopping, with how long it ran, and closes the ring: after the core has closed, so nothing
// it writes is lost. A no-op on nil.
func (self *sidecarSupportLog) close() {
	if self == nil {
		return
	}
	self.log.Event(sidecar.SupportLogCodeAppStopped, sidecar.SupportLogFieldUptime(self.clock.Now().Sub(self.started)))
	self.log.Close()
}
