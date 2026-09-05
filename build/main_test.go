package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestAndroidBuildOutputLockExcludesConcurrentWriter exercises the real
// kernel-backed gate without running gomobile. A FIFO makes ownership
// deterministic: the first command publishes readiness only after acquiring
// the lock, then remains blocked until the test releases it.
func TestAndroidBuildOutputLockExcludesConcurrentWriter(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("Android SDK output locking supports Darwin and Linux build hosts")
	}
	tempDir := t.TempDir()
	gatePath, err := filepath.Abs("sdk-android-output-lock.sh")
	testingBuildNoError(t, err)
	makefilePath, err := filepath.Abs("Makefile")
	testingBuildNoError(t, err)
	releasePath := filepath.Join(tempDir, "release")
	testingBuildCommand(t, tempDir, "mkfifo", releasePath)
	readyPath := filepath.Join(tempDir, "ready")

	owner := exec.Command(
		gatePath,
		"first-owner",
		"--",
		"/bin/sh",
		"-c",
		`printf "ready\n" >"$1"; IFS= read -r _ <"$2"`,
		"sh",
		readyPath,
		releasePath,
	)
	owner.Dir = tempDir
	owner.Env = testingBuildEnvironmentWithoutAndroidLock()
	var ownerOutput bytes.Buffer
	owner.Stdout = &ownerOutput
	owner.Stderr = &ownerOutput
	testingBuildNoError(t, owner.Start())
	ownerDone := false
	t.Cleanup(func() {
		if !ownerDone {
			_ = owner.Process.Kill()
			_ = owner.Wait()
		}
	})
	testingBuildWaitForFile(t, readyPath)

	shouldNotRun := filepath.Join(tempDir, "contender-ran")
	contender := exec.Command(
		gatePath,
		"second-owner",
		"--",
		"/bin/sh",
		"-c",
		`touch "$1"`,
		"sh",
		shouldNotRun,
	)
	contender.Dir = tempDir
	contender.Env = testingBuildEnvironmentWithoutAndroidLock()
	contenderOutput, contenderErr := contender.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(contenderErr, &exitError) || exitError.ExitCode() != 75 {
		t.Fatalf("contending writer returned %v, want exit 75:\n%s", contenderErr, contenderOutput)
	}
	if _, err := os.Stat(shouldNotRun); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("contending writer ran without the lock: %v", err)
	}
	if !strings.Contains(string(contenderOutput), "busy (role=first-owner pid=") {
		t.Fatalf("contention did not report sanitized owner metadata:\n%s", contenderOutput)
	}
	build := exec.Command("make", "-f", makefilePath, "build_android")
	build.Dir = tempDir
	build.Env = append(
		testingBuildEnvironmentWithoutAndroidLock(),
		"ANDROID_NDK_HOME="+filepath.Join(tempDir, "empty-ndk"),
		"WARP_VERSION=test",
	)
	buildOutput, buildErr := build.CombinedOutput()
	if buildErr == nil {
		t.Fatalf("contending public Android build unexpectedly acquired output ownership:\n%s", buildOutput)
	}
	if !strings.Contains(string(buildOutput), "Android SDK output gate: busy") {
		t.Fatalf("contending public Android build failed for the wrong reason:\n%s", buildOutput)
	}

	// `clean` deletes Android staging and published directories, so it is a
	// writer under the same lock rather than a harmless maintenance command.
	protectedOutput := filepath.Join(tempDir, "android.protected")
	testingBuildNoError(t, os.Mkdir(protectedOutput, 0o700))
	clean := exec.Command("make", "-f", makefilePath, "clean")
	clean.Dir = tempDir
	clean.Env = testingBuildEnvironmentWithoutAndroidLock()
	cleanOutput, cleanErr := clean.CombinedOutput()
	if cleanErr == nil {
		t.Fatalf("contending clean unexpectedly acquired output ownership:\n%s", cleanOutput)
	}
	if !strings.Contains(string(cleanOutput), "Android SDK output gate: busy") {
		t.Fatalf("contending clean failed for the wrong reason:\n%s", cleanOutput)
	}
	if _, err := os.Stat(protectedOutput); err != nil {
		t.Fatalf("contending clean mutated Android output: %v", err)
	}

	testingBuildNoError(t, os.WriteFile(releasePath, []byte("release\n"), 0o600))
	if err := owner.Wait(); err != nil {
		t.Fatalf("first lock owner failed: %v\n%s", err, ownerOutput.Bytes())
	}
	ownerDone = true

	// The metadata file is deliberately persistent. Without a live kernel lock,
	// stale content must not block the next writer.
	lockPath := filepath.Join(tempDir, ".android-output.lock")
	testingBuildNoError(t, os.WriteFile(
		lockPath,
		[]byte("version=1\nrole=stale-owner\npid=999999\n"),
		0o600,
	))
	replacement := exec.Command(gatePath, "replacement-owner", "--", "/usr/bin/true")
	replacement.Dir = tempDir
	replacement.Env = testingBuildEnvironmentWithoutAndroidLock()
	if output, err := replacement.CombinedOutput(); err != nil {
		t.Fatalf("stale metadata blocked a replacement writer: %v\n%s", err, output)
	}
	metadata, err := os.ReadFile(lockPath)
	testingBuildNoError(t, err)
	if !strings.Contains(string(metadata), "role=replacement-owner\n") {
		t.Fatalf("replacement ownership metadata was not recorded:\n%s", metadata)
	}

	// A nested tracked launcher inherits descriptor ownership instead of
	// deadlocking itself or opening an independently lockable file.
	nestedMarker := filepath.Join(tempDir, "nested-ran")
	nested := exec.Command(
		gatePath,
		"outer-owner",
		"--",
		gatePath,
		"inner-owner",
		"--",
		"/bin/sh",
		"-c",
		`touch "$1"`,
		"sh",
		nestedMarker,
	)
	nested.Dir = tempDir
	nested.Env = testingBuildEnvironmentWithoutAndroidLock()
	if output, err := nested.CombinedOutput(); err != nil {
		t.Fatalf("nested tracked writer did not inherit lock ownership: %v\n%s", err, output)
	}
	if _, err := os.Stat(nestedMarker); err != nil {
		t.Fatalf("nested tracked command did not run: %v", err)
	}
}

// TestAndroidBuildOutputLockReleasesOnOwnerDeath proves that cleanup does not
// depend on PID guessing or deleting a possibly live owner's lock file.
func TestAndroidBuildOutputLockReleasesOnOwnerDeath(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("Android SDK output locking supports Darwin and Linux build hosts")
	}
	tempDir := t.TempDir()
	gatePath, err := filepath.Abs("sdk-android-output-lock.sh")
	testingBuildNoError(t, err)
	releasePath := filepath.Join(tempDir, "release")
	testingBuildCommand(t, tempDir, "mkfifo", releasePath)
	readyPath := filepath.Join(tempDir, "ready")
	owner := exec.Command(
		gatePath,
		"killed-owner",
		"--",
		"/bin/sh",
		"-c",
		`printf "ready\n" >"$1"; IFS= read -r _ <"$2"`,
		"sh",
		readyPath,
		releasePath,
	)
	owner.Dir = tempDir
	owner.Env = testingBuildEnvironmentWithoutAndroidLock()
	testingBuildNoError(t, owner.Start())
	testingBuildWaitForFile(t, readyPath)
	testingBuildNoError(t, owner.Process.Kill())
	if err := owner.Wait(); err == nil {
		t.Fatal("killed lock owner unexpectedly exited successfully")
	}

	afterKill := exec.Command(gatePath, "after-kill", "--", "/usr/bin/true")
	afterKill.Dir = tempDir
	afterKill.Env = testingBuildEnvironmentWithoutAndroidLock()
	if output, err := afterKill.CombinedOutput(); err != nil {
		t.Fatalf("kernel lock remained held after owner death: %v\n%s", err, output)
	}
}

// TestAndroidBuildOutputLockSourceRetainsConsumerOwnership covers the API used
// by Android acceptance: sourcing the gate keeps descriptor 8 locked in the
// runner shell while later Gradle consumers execute.
func TestAndroidBuildOutputLockSourceRetainsConsumerOwnership(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("Android SDK output locking supports Darwin and Linux build hosts")
	}
	tempDir := t.TempDir()
	gatePath, err := filepath.Abs("sdk-android-output-lock.sh")
	testingBuildNoError(t, err)
	releasePath := filepath.Join(tempDir, "release")
	testingBuildCommand(t, tempDir, "mkfifo", releasePath)
	readyPath := filepath.Join(tempDir, "ready")

	owner := exec.Command(
		"/bin/bash",
		"-c",
		`set -euo pipefail
cd "$1"
source "$2"
sdk_android_output_lock_acquire android-acceptance-consumer
"$2" --verify-held
printf "ready\n" >"$3"
IFS= read -r _ <"$4"`,
		"bash",
		tempDir,
		gatePath,
		readyPath,
		releasePath,
	)
	owner.Env = testingBuildEnvironmentWithoutAndroidLock()
	var ownerOutput bytes.Buffer
	owner.Stdout = &ownerOutput
	owner.Stderr = &ownerOutput
	testingBuildNoError(t, owner.Start())
	ownerDone := false
	t.Cleanup(func() {
		if !ownerDone {
			_ = owner.Process.Kill()
			_ = owner.Wait()
		}
	})
	testingBuildWaitForFile(t, readyPath)

	contender := exec.Command(gatePath, "writer-during-consumption", "--", "/usr/bin/true")
	contender.Dir = tempDir
	contender.Env = testingBuildEnvironmentWithoutAndroidLock()
	output, err := contender.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 75 {
		t.Fatalf("writer was not excluded by sourced consumer ownership: %v\n%s", err, output)
	}

	testingBuildNoError(t, os.WriteFile(releasePath, []byte("release\n"), 0o600))
	if err := owner.Wait(); err != nil {
		t.Fatalf("sourced consumer owner failed: %v\n%s", err, ownerOutput.Bytes())
	}
	ownerDone = true
}

// TestAndroidBuildOutputLockRejectsForgedInheritance proves that a generic
// environment marker cannot bypass descriptor-inode and random-token checks.
func TestAndroidBuildOutputLockRejectsForgedInheritance(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("Android SDK output locking supports Darwin and Linux build hosts")
	}
	tempDir := t.TempDir()
	gatePath, err := filepath.Abs("sdk-android-output-lock.sh")
	testingBuildNoError(t, err)
	lockPath := filepath.Join(tempDir, ".android-output.lock")
	const realToken = "0123456789abcdef0123456789abcdef"
	testingBuildNoError(t, os.WriteFile(lockPath, []byte(
		"version=1\nrole=real-owner\npid=123\ntoken="+realToken+
			"\nstarted_utc=2026-09-05T00:00:00Z\n"), 0o600))

	tests := []struct {
		name       string
		descriptor string
		token      string
	}{
		{name: "wrong-token", descriptor: lockPath, token: strings.Repeat("a", 32)},
		{name: "wrong-inode", descriptor: filepath.Join(tempDir, "other"), token: realToken},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command(
				"/bin/bash",
				"-c",
				`set -euo pipefail
exec 8>>"$1"
export URNETWORK_ANDROID_SDK_OUTPUT_LOCK_HELD=1
export URNETWORK_ANDROID_SDK_OUTPUT_LOCK_DIR="$2"
export URNETWORK_ANDROID_SDK_OUTPUT_LOCK_PATH="$3"
export URNETWORK_ANDROID_SDK_OUTPUT_LOCK_TOKEN="$4"
export URNETWORK_ANDROID_SDK_OUTPUT_LOCK_ROLE=forged
exec "$5" --verify-held`,
				"bash",
				test.descriptor,
				tempDir,
				lockPath,
				test.token,
				gatePath,
			)
			command.Dir = tempDir
			command.Env = testingBuildEnvironmentWithoutAndroidLock()
			output, err := command.CombinedOutput()
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != 70 {
				t.Fatalf("forged inheritance returned %v, want exit 70:\n%s", err, output)
			}
			if bytes.Contains(output, []byte(realToken)) || bytes.Contains(output, []byte(test.token)) {
				t.Fatalf("ownership token leaked in diagnostic output: %s", output)
			}
		})
	}
}

// TestAndroidBuildOutputLockPreservesOuterDescriptorNine guards integration
// with the canonical suite lock: acquiring and retaining SDK descriptor 8 may
// neither close nor replace the independently kernel-locked descriptor 9.
func TestAndroidBuildOutputLockPreservesOuterDescriptorNine(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("Android SDK output locking supports Darwin and Linux build hosts")
	}
	tempDir := t.TempDir()
	gatePath, err := filepath.Abs("sdk-android-output-lock.sh")
	testingBuildNoError(t, err)
	outerPath := filepath.Join(tempDir, "outer.lock")
	releasePath := filepath.Join(tempDir, "release")
	readyPath := filepath.Join(tempDir, "ready")
	testingBuildCommand(t, tempDir, "mkfifo", releasePath)

	owner := exec.Command(
		"/bin/bash",
		"-c",
		`set -euo pipefail
cd "$1"
exec 9>>"$2"
case "$3" in
  darwin) /usr/bin/lockf -s -t 0 9 ;;
  linux) flock -n 9 ;;
esac
source "$4"
sdk_android_output_lock_acquire android-acceptance-consumer
sdk_android_output_lock_verify_held
: >&9
case "$3" in
  darwin)
    /usr/bin/perl -e '
      open(my $lock, ">&=9") or exit 1;
      my @descriptor = stat($lock);
      my @path = stat($ARGV[0]);
      exit(!(@descriptor && @path &&
        $descriptor[0] == $path[0] && $descriptor[1] == $path[1]));
    ' "$2"
    /usr/bin/lockf -s -t 0 9
    ;;
  linux)
    [ "/proc/$$/fd/9" -ef "$2" ]
    flock -n 9
    ;;
esac
printf "ready\n" >"$5"
IFS= read -r _ <"$6"`,
		"bash",
		tempDir,
		outerPath,
		runtime.GOOS,
		gatePath,
		readyPath,
		releasePath,
	)
	owner.Env = testingBuildEnvironmentWithoutAndroidLock()
	var ownerOutput bytes.Buffer
	owner.Stdout = &ownerOutput
	owner.Stderr = &ownerOutput
	testingBuildNoError(t, owner.Start())
	ownerDone := false
	t.Cleanup(func() {
		if !ownerDone {
			_ = owner.Process.Kill()
			_ = owner.Wait()
		}
	})
	testingBuildWaitForFile(t, readyPath)

	sdkContender := exec.Command(gatePath, "nested-sdk-contender", "--", "/usr/bin/true")
	sdkContender.Dir = tempDir
	sdkContender.Env = testingBuildEnvironmentWithoutAndroidLock()
	if output, err := sdkContender.CombinedOutput(); err == nil {
		t.Fatalf("SDK contender acquired descriptor 8 while both locks were held:\n%s", output)
	}
	outerContender := exec.Command(
		"/bin/bash",
		"-c",
		`exec 9>>"$1"
case "$2" in
  darwin) /usr/bin/lockf -s -t 0 9 ;;
  linux) flock -n 9 ;;
esac`,
		"bash",
		outerPath,
		runtime.GOOS,
	)
	if output, err := outerContender.CombinedOutput(); err == nil {
		t.Fatalf("outer contender acquired descriptor 9 while both locks were held:\n%s", output)
	}

	testingBuildNoError(t, os.WriteFile(releasePath, []byte("release\n"), 0o600))
	if err := owner.Wait(); err != nil {
		t.Fatalf("nested descriptor owner failed: %v\n%s", err, ownerOutput.Bytes())
	}
	ownerDone = true
}

// TestAndroidBuildInnerTargetRequiresOutputLock keeps the mutating recipe from
// becoming an undocumented bypass around the public, locked build target.
func TestAndroidBuildInnerTargetRequiresOutputLock(t *testing.T) {
	tempDir := t.TempDir()
	makefilePath, err := filepath.Abs("Makefile")
	testingBuildNoError(t, err)
	for _, target := range []string{"_build_android", "_clean", "_init", "_init_tools"} {
		command := exec.Command("make", "-f", makefilePath, target)
		command.Dir = tempDir
		command.Env = testingBuildEnvironmentWithoutAndroidLock()
		output, err := command.CombinedOutput()
		if err == nil {
			t.Fatalf("unguarded Android SDK target %s unexpectedly ran:\n%s", target, output)
		}
		if !strings.Contains(string(output), "inherited ownership does not match this output") {
			t.Fatalf("unguarded target %s failed for the wrong reason:\n%s", target, output)
		}
	}
}

// TestAndroidBuildMakefileGatesEveryTrackedMutation prevents a future launcher
// refactor from leaving build, cleanup, or module initialization outside the
// shared ownership boundary.
func TestAndroidBuildMakefileGatesEveryTrackedMutation(t *testing.T) {
	makefileBytes, err := os.ReadFile("Makefile")
	testingBuildNoError(t, err)
	makefile := string(makefileBytes)
	for _, contract := range []struct {
		target string
		role   string
	}{
		{target: "build_android", role: "sdk-build-android"},
		{target: "clean", role: "sdk-clean"},
		{target: "init", role: "sdk-init-android"},
		{target: "init_tools", role: "sdk-init-android-tools"},
	} {
		expected := contract.target + ":\n\t@\"$(ANDROID_OUTPUT_GATE)\" " + contract.role + " --"
		if !strings.Contains(makefile, expected) {
			t.Errorf("Make target %s does not acquire the Android output gate", contract.target)
		}
	}
	for _, innerTarget := range []string{"_build_android", "_clean", "_init", "_init_tools"} {
		expected := innerTarget + ":\n\t@\"$(ANDROID_OUTPUT_GATE)\" --verify-held"
		if !strings.Contains(makefile, expected) {
			t.Errorf("Make target %s does not revalidate inherited kernel ownership", innerTarget)
		}
	}
	for _, provenanceContract := range []string{
		"URNETWORK_ANDROID_SDK_BUILD_OWNER",
		`printf '%s\n' "$$BUILD_OWNER" >"$$BUILD_DIR/.build-owner"`,
	} {
		if !strings.Contains(makefile, provenanceContract) {
			t.Errorf("Android SDK publication lacks provenance contract %q", provenanceContract)
		}
	}
	gateInfo, err := os.Stat("sdk-android-output-lock.sh")
	testingBuildNoError(t, err)
	if gateInfo.Mode()&0o111 == 0 {
		t.Fatal("Android output gate is not executable")
	}
	ignoreBytes, err := os.ReadFile(".gitignore")
	testingBuildNoError(t, err)
	if !strings.Contains(string(ignoreBytes), ".android-output.lock\n") {
		t.Fatal("persistent Android output lock metadata is not ignored")
	}
}

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
	command.Env = append(testingBuildEnvironmentWithoutAndroidLock(),
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
	command.Env = append(os.Environ(), "GOWORK=off")
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

func testingBuildEnvironmentWithoutAndroidLock() []string {
	environment := make([]string, 0, len(os.Environ()))
	for _, item := range os.Environ() {
		if strings.HasPrefix(item, "URNETWORK_ANDROID_SDK_OUTPUT_LOCK_HELD=") ||
			strings.HasPrefix(item, "URNETWORK_ANDROID_SDK_OUTPUT_LOCK_DIR=") ||
			strings.HasPrefix(item, "URNETWORK_ANDROID_SDK_OUTPUT_LOCK_PATH=") ||
			strings.HasPrefix(item, "URNETWORK_ANDROID_SDK_OUTPUT_LOCK_TOKEN=") ||
			strings.HasPrefix(item, "URNETWORK_ANDROID_SDK_OUTPUT_LOCK_ROLE=") ||
			strings.HasPrefix(item, "URNETWORK_ANDROID_SDK_BUILD_OWNER=") {
			continue
		}
		environment = append(environment, item)
	}
	return environment
}

func testingBuildCommand(t *testing.T, directory string, name string, arguments ...string) {
	t.Helper()
	command := exec.Command(name, arguments...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s failed: %v\n%s", name, err, output)
	}
}

func testingBuildWaitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("could not inspect readiness file: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
