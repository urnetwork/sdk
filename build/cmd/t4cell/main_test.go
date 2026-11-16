package main

// The runner builds an Android surrogate's cells as the app's bind builds the sdk, and the binary it builds holds every
// cell it runs (SDK-M8-15). The sdk's tests once named root types that phones' builds do not link, the daemon's among
// them, so no Android cell binary built at all; this builds one here, where no device is needed.

import (
	"debug/elf"
	"path/filepath"
	"testing"
)

func TestAndroidCellsBuildAsTheBindBuilds(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "sdk.test")
	build, err := cellBuild(t4TargetAndroid, binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	binary, err := elf.Open(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer binary.Close()
	if binary.Machine != elf.EM_AARCH64 {
		t.Errorf("the cells built for %v", binary.Machine)
	}
	symbols, err := binary.Symbols()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, symbol := range symbols {
		names[symbol.Name] = true
	}
	for _, cell := range t4CellTests {
		if !names[t4CellPackage+"."+cell] {
			t.Errorf("the Android binary holds no %s", cell)
		}
	}
}

// A target the runner does not know is refused before anything builds.
func TestUnknownTargetRefused(t *testing.T) {
	if _, err := cellBuild("ios", filepath.Join(t.TempDir(), "sdk.test")); err == nil {
		t.Error("an ios target built: its surrogate's run needs a signed test host")
	}
}
