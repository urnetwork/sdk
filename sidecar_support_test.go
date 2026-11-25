//go:build !ios_extension && !js

package sdk

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
)

// A local Sidecar keeps the app process's support log under its state directory, recording its start and, once the
// core has closed, its stop (DESIGN-COMPUTE §28.7).
func TestSidecarLocalKeepsTheAppsSupportLog(t *testing.T) {
	world := newTestSidecarWorld(t)
	local := world.open(t)
	if local.supportLog == nil {
		t.Fatal("a local Sidecar with a state directory opened no support log")
	}
	world.close(t, local)

	reopened, err := sidecar.OpenSupportLog(&sidecar.SupportLogArgs{
		Directory: sidecar.SupportLogDirectory(world.stateDir),
		Settings:  sidecar.DefaultSupportSettings(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	bundle, err := reopened.Bundle(nil)
	if err != nil {
		t.Fatal(err)
	}
	started, stopped := 0, 0
	for _, event := range bundle.Document.Events {
		switch {
		case event.Component == "app" && event.Code == "started":
			started += 1
		case event.Component == "app" && event.Code == "stopped":
			stopped += 1
		}
	}
	if started != 1 || stopped != 1 {
		t.Fatalf("%d app.started and %d app.stopped", started, stopped)
	}
}

// A Sidecar with no state directory to keep one in opens none, and still runs.
func TestSidecarSupportLogNeedsAnAbsoluteStateDirectory(t *testing.T) {
	coreSettings := sidecar.DefaultSidecarSettings()
	if log := openSidecarSupportLog("", coreSettings); log != nil || coreSettings.SupportLog != nil {
		t.Fatal("a ring opened with no state directory")
	}
	if log := openSidecarSupportLog("relative/state", coreSettings); log != nil || coreSettings.SupportLog != nil {
		t.Fatal("a ring opened under a relative directory")
	}
	var none *sidecarSupportLog
	none.close()
}

// A Sidecar whose key store is locked writes nothing of its ring into the state directory; once the instance opens,
// the ring writes what it held, the app's start among it.
func TestSidecarSupportLogWaitsForTheInstanceToOpen(t *testing.T) {
	world := newTestSidecarWorld(t)
	world.keyStore.setMode(testKeyStoreLocked)
	local := world.open(t)
	awaitTestSidecarState(t, local, sidecar.SidecarStateLocked, "")
	if local.supportLog == nil {
		t.Fatal("a local Sidecar with a state directory holds no support log")
	}
	select {
	case <-local.supportLog.writing:
		t.Fatal("the ring writes while the key store is locked")
	default:
	}
	world.requireNothingCreated(t)

	world.keyStore.setMode(testKeyStoreWorking)
	if err := local.Core().Unlock(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-local.supportLog.writing:
	case <-time.After(testSidecarTimeout):
		t.Fatal("the ring never wrote once the instance opened")
	}
	if _, err := os.Stat(filepath.Join(sidecar.SupportLogDirectory(world.stateDir), "key")); err != nil {
		t.Fatal(err)
	}
	bundle, err := local.supportLog.log.Bundle(nil)
	if err != nil {
		t.Fatal(err)
	}
	started := 0
	for _, event := range bundle.Document.Events {
		if event.Component == "app" && event.Code == "started" {
			started += 1
		}
	}
	if started != 1 {
		t.Fatalf("%d app.started once the ring wrote", started)
	}
}
