//go:build !ios_extension && !js

// The support skill in the apps (DESIGN-COMPUTE §28): the app process's support log ring (§28.7), opened beside the
// local core under the sidecar's state directory, which the core writes through and which records the app's sidecar
// starting and stopping. The app's support conversation, Support() (§15.3, §28.4), is sidecar_support_conversation.go's.
//
// The ring is the app process's own, as ur daemon's is the daemon's: the VPN component, which runs only forwarding
// (owner decision 197), keeps none, and a remote Sidecar writes through its daemon's. It opens deferred and writes
// nothing into the state directory until the instance has opened, so a sidecar whose key store is locked or failed
// creates nothing there, as the key store's rule asks (sidecar_keystore_test.go); what it held meanwhile, the app's
// start among it, goes to disk with its times once the instance opens, and a sidecar closed before that writes none.
// One that cannot open leaves the app without one: the support log is never a reason to stop work (§28.14).

package sdk

import (
	"context"
	"path/filepath"
	"sync"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
)

// A local Sidecar's ring and when it opened.
type sidecarSupportLog struct {
	log     *sidecar.SupportLog
	clock   sidecar.Clock
	started time.Time
	// closed once the ring writes to disk: what a test waits on
	writing     chan struct{}
	writingOnce sync.Once
}

// The ring under stateDir, deferred, set as coreSettings' support log, with the app's start recorded; nil where
// stateDir is no absolute directory or the ring cannot open.
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
		Deferred:  true,
	})
	if err != nil {
		return nil
	}
	coreSettings.SupportLog = log
	log.Event(sidecar.SupportLogCodeAppStarted, sidecar.SupportLogFieldRelease(Version))
	return &sidecarSupportLog{log: log, clock: clock, started: clock.Now(), writing: make(chan struct{})}
}

// Starts the ring's writes once instance is open: at once where it is, else when it opens, until ctx ends. A no-op on
// nil.
func (self *sidecarSupportLog) startWhenOpen(ctx context.Context, instance *sidecar.SidecarInstance) {
	if self == nil {
		return
	}
	state, changed := instance.State()
	if state == sidecar.SidecarInstanceOpen {
		self.start()
		return
	}
	go connect.HandleError(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-changed:
			}
			state, changed = instance.State()
			if state == sidecar.SidecarInstanceOpen {
				self.start()
				return
			}
		}
	})
}

// Starts the ring's writes, telling writing once they do.
func (self *sidecarSupportLog) start() {
	if err := self.log.Start(); err == nil {
		self.writingOnce.Do(func() { close(self.writing) })
	}
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
