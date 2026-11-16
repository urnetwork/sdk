//go:build !ios_extension && !js

package sdk

// T4's ios32 and Android surrogate cells (SDK-M8-15; §22.4's T4, §22.9's Memory, size row; §18.4's budgets): a cell
// boots the app's sidecar role under its profile, as the app does, and holds its peak and its steady heap with the role
// open to §18.4's budget for the class; the Android cell runs no background sidecar, whatever the VPN service does
// (owner decision 197), and only an OS window or a wake lets it run with the app closed. The cells run on this host
// under the profiles' Go memory limits; the surrogate devices' runs of the same cells are external.

import (
	"context"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/t4cell"
)

// Boots the app's sidecar role on cell's profile under class: its peak while it opens and its steady heap once open,
// each held to the profile's sidecar budget.
func testT4BootCell(t *testing.T, cell *t4cell.Cell, class string) *Sidecar {
	t.Helper()
	world := newTestSidecarWorld(t)
	budget := cell.Profile().SidecarBudgetByteCount
	var local *Sidecar
	if _, err := cell.Measure("sidecar role boots ("+class+")", budget, t4cell.HoldsPeak, func() (any, error) {
		settings := DefaultSidecarSettings()
		settings.HostClass = class
		settings.StateDir = world.stateDir
		settings.Components.Add(SidecarComponentEvs)
		opened, err := newSidecarLocal(context.Background(), world.space, world.keyStore, nil, settings, &sidecarLocalOptions{
			componentFactories:  map[string]sidecarComponentFactory{SidecarComponentEvs: testSearchEvsComponent},
			searchFreeByteCount: func(dir string) (uint64, error) { return 8 * 1024 * 1024 * 1024, nil },
		})
		local = opened
		return opened, err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		world.close(t, local)
	})
	if _, err := cell.Measure("sidecar role open, steady ("+class+")", budget, t4cell.HoldsSteady, func() (any, error) {
		if local.GetSearch() == nil {
			t.Error("the booted role has no search")
		}
		return local, nil
	}); err != nil {
		t.Fatal(err)
	}
	return local
}

// The ios32 cell boots the iOS app's sidecar role within §18.4's 32 MiB, its peak and its steady heap.
func TestIos32CellBootsWithinItsBudget(t *testing.T) {
	cell := t4cell.NewCell(t4cell.Ios32Profile())
	local := testT4BootCell(t, cell, SidecarHostClassIos)
	if local.hostClass.name != SidecarHostClassIos || local.hostClass.memoryBudgetByteCount != cell.Profile().SidecarBudgetByteCount {
		t.Errorf("the role's class %+v", local.hostClass)
	}
	if err := cell.CheckWithin(); err != nil {
		t.Error(err)
	}
	t.Log("\n" + cell.Table())
}

// The Android cell boots the sidecar role within its 16 MiB added, and runs no background sidecar: with the app
// closed and the VPN service running it may not run, and only an OS-granted window or a wake lets it (owner decision
// 197).
func TestAndroidCellRunsNoBackgroundSidecar(t *testing.T) {
	cell := t4cell.NewCell(t4cell.AndroidProfile())
	local := testT4BootCell(t, cell, SidecarHostClassAndroid)
	if err := cell.CheckWithin(); err != nil {
		t.Error(err)
	}
	t.Log("\n" + cell.Table())
	host := local.searchHost
	search := local.GetSearch()
	search.SetForeground(true)
	if !host.mayRun(false) {
		t.Fatal("the app in the foreground may not run its sidecar")
	}
	search.SetForeground(false)
	if host.mayRun(false) {
		t.Error("the Android cell runs a background sidecar with the app closed")
	}
	if local.hostClass.mayRun(false, false, false, true) {
		t.Error("the VPN service running lets the Android sidecar run")
	}
	if !host.mayRun(true) {
		t.Error("a wake does not let the sidecar run")
	}
	closeWindow := host.openWindow()
	if !host.mayRun(false) {
		t.Error("an OS window does not let the sidecar run")
	}
	closeWindow()
	if host.mayRun(false) {
		t.Error("the window outlived its close")
	}
}
