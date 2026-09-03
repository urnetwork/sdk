package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// Keep the gomobile toolchain in the tidy module graph at the exact version
// used by init, so preparing a release build never rewrites go.mod or go.sum.
func TestMobileBuildToolsRemainPinnedAndTidy(t *testing.T) {
	const gomobileVersion = "v0.0.0-20260820023541-8e8303b9da6c"
	moduleBytes, err := os.ReadFile("go.mod")
	testingBuildNoError(t, err)
	module := string(moduleBytes)
	for _, tool := range []string{"golang.org/x/mobile/cmd/gobind", "golang.org/x/mobile/cmd/gomobile"} {
		if !strings.Contains(module, "\t"+tool+"\n") {
			t.Errorf("mobile build module does not retain tool %s", tool)
		}
	}
	if !strings.Contains(module, "golang.org/x/mobile "+gomobileVersion+" // indirect") {
		t.Fatalf("mobile build module does not pin x/mobile %s", gomobileVersion)
	}
	makefileBytes, err := os.ReadFile("Makefile")
	testingBuildNoError(t, err)
	makefile := string(makefileBytes)
	if !strings.Contains(makefile, "GOMOBILE_VERSION ?= "+gomobileVersion) ||
		!strings.Contains(makefile, "go get golang.org/x/mobile/bind@$(GOMOBILE_VERSION)") {
		t.Fatal("mobile init and module tool versions can drift apart")
	}
	command := exec.Command("go", "mod", "tidy", "-diff")
	command.Dir = "."
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("mobile build module is not tidy: %v\n%s", err, output)
	}
}

// testingBuildNoError fails the current build regression test on fixture errors.
func testingBuildNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
