package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWindowsManifestUsesFreshVMArchive(t *testing.T) {
	t.Setenv("WINDOWS_BUILD_ARCHITECTURES", "amd64,arm64")
	base := t.TempDir()
	source := filepath.Join(base, "source")
	destination := filepath.Join(base, "out")
	for _, arch := range []string{"amd64", "arm64"} {
		textFile(filepath.Join(source, "windows", arch, "URnetworkSdk.dll"), "new-"+arch)
		textFile(filepath.Join(destination, "windows", arch, "URnetworkSdk.dll"), "stale")
	}
	archive := filepath.Join(base, "sdk.zip")
	zipTree(source, archive)
	start := time.Now().Add(-time.Minute)
	stageWindowsRuntime(archive, destination, start)
	for _, arch := range []string{"amd64", "arm64"} {
		if string(read(filepath.Join(destination, "windows", arch, "URnetworkSdk.dll"))) != "new-"+arch {
			t.Fatal("stale DLL survived")
		}
	}
	if err := os.Chtimes(archive, start.Add(-time.Hour), start.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("stale archive accepted")
		}
	}()
	stageWindowsRuntime(archive, destination, start)
}

func TestWindowsArchiveMustContainEverySelectedArchitecture(t *testing.T) {
	t.Setenv("WINDOWS_BUILD_ARCHITECTURES", "amd64,arm64")
	base := t.TempDir()
	source := filepath.Join(base, "source")
	textFile(filepath.Join(source, "windows/amd64/URnetworkSdk.dll"), "amd64")
	archive := filepath.Join(base, "sdk.zip")
	zipTree(source, archive)
	defer func() {
		if recover() == nil {
			t.Fatal("missing arm64 accepted")
		}
	}()
	stageWindowsRuntime(archive, filepath.Join(base, "out"), time.Now().Add(-time.Minute))
}
