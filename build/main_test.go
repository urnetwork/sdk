package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestAndroidBuildStopsAfterGomobileFailure verifies that a failed binder cannot
// fall through into artifact editing or replace the last known-good Android SDK.
func TestAndroidBuildStopsAfterGomobileFailure(t *testing.T) {
	tempDir := t.TempDir()
	binDir := filepath.Join(tempDir, "bin")
	ndkDir := filepath.Join(tempDir, "ndk")
	testingBuildNoError(t, os.Mkdir(binDir, 0o755))
	testingBuildNoError(t, os.Mkdir(ndkDir, 0o755))
	testingBuildNoError(t, os.WriteFile(
		filepath.Join(ndkDir, "llvm-objcopy"),
		[]byte("#!/bin/sh\nexit 0\n"),
		0o755,
	))
	testingBuildNoError(t, os.WriteFile(
		filepath.Join(binDir, "gomobile"),
		[]byte("#!/bin/sh\nexit 23\n"),
		0o755,
	))
	testingBuildNoError(t, os.WriteFile(
		filepath.Join(binDir, "unzip"),
		[]byte("#!/bin/sh\ntouch \"$UNZIP_MARKER\"\nexit 0\n"),
		0o755,
	))
	makefilePath, err := filepath.Abs("Makefile")
	testingBuildNoError(t, err)
	unzipMarkerPath := filepath.Join(tempDir, "unzip-called")
	command := exec.Command("make", "-f", makefilePath, "build_android")
	command.Dir = tempDir
	command.Env = append(os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"ANDROID_NDK_HOME="+ndkDir,
		"UNZIP_MARKER="+unzipMarkerPath,
		"WARP_VERSION=test",
	)

	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("build unexpectedly succeeded after gomobile failure:\n%s", output)
	}
	if _, err := os.Stat(unzipMarkerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("build continued into unzip after gomobile failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tempDir, "android")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("build replaced the android artifact after gomobile failure: %v", err)
	}
}

// testingBuildNoError fails the current build regression test on fixture errors.
func testingBuildNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
