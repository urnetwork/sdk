// The T4 cell runner (SDK-M8-15; sidecar §22.4's T4, §22.9's Memory, size row; owner decision 100): builds the sdk's
// cell tests (sidecar_t4_cells_test.go, sidecar_memory_t4_test.go) for a target and runs them there, printing each
// cell's rows: on this host, or on an Android surrogate attached to adb, where the same cells measure the device. A
// target that cannot run here reports not run, which a gate lists as open and never as a pass (§22.4's T1 rule).
//
// An Android surrogate's cells build as the app's bind builds the sdk (build/Makefile's android target): the
// sdk_mobile_bind tag, which selects the mobile files of the sdk and of the sidecar module, and the linker's
// -checklinkname=0, which the Android network helper's linkname into net needs. Without cgo, since no NDK is needed
// to build or run a test binary there.
//
// The cells boot the app's sidecar role on a network space that discovers nothing, and enroll, take a wake and read
// against test servers on loopback, so a run reaches no host. The iOS surrogate's run needs a signed test host and is
// external. An Android run takes the device named by -serial and no other: a device attached to this machine may be
// another campaign's (tests/runner/perf's physical custody), so the runner never picks one itself.
//
//	go run ./cmd/t4cell -target host
//	go run ./cmd/t4cell -target android -build-only
//	go run ./cmd/t4cell -target android -serial <the surrogate's adb serial>
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The cells, by test name, and the presence vectors FS-124 holds byte-equal on every leg.
var t4CellTests = []string{
	"TestIos32CellBootsWithinItsBudget",
	"TestAndroidCellRunsNoBackgroundSidecar",
	"TestT4BudgetCatchesADeliberateOverAllocation",
	"TestForwardingCompileAdmissionOnTheIos32Cell",
	"TestIos32CellEnrollsTakesAWakeAndReads",
	"TestAndroidCellEnrollsTakesAWakeAndReads",
	"TestPresenceVectorsCrossTheAppSignerByteEqual",
}

// The targets, as -target names them.
const (
	t4TargetHost    = "host"
	t4TargetAndroid = "android"
)

// Where an Android surrogate's run keeps its binary.
const t4AndroidDirectory = "/data/local/tmp"

// The package the cells are tests of.
const t4CellPackage = "github.com/urnetwork/sdk"

func main() {
	target := flag.String("target", t4TargetHost, "host, or android for a surrogate attached to adb")
	serial := flag.String("serial", "", "android: the surrogate's adb serial; required, since no device is picked")
	buildOnly := flag.Bool("build-only", false, "build the cells for the target and run nothing")
	flag.Parse()
	if err := run(*target, *serial, *buildOnly); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// The command that builds the cells' test binary for target at binaryPath, as the target's bind builds the sdk.
func cellBuild(target string, binaryPath string) (*exec.Cmd, error) {
	switch target {
	case t4TargetHost:
		return exec.Command("go", "test", "-c", "-o", binaryPath, t4CellPackage), nil
	case t4TargetAndroid:
		build := exec.Command("go", "test", "-c", "-o", binaryPath, "-tags", "sdk_mobile_bind", "-ldflags", "-checklinkname=0", t4CellPackage)
		build.Env = append(os.Environ(), "GOOS=android", "GOARCH=arm64", "CGO_ENABLED=0")
		return build, nil
	}
	return nil, errors.New("a target is host or android")
}

// Builds the cells for target and runs them there: on android, on the device serial names. buildOnly stops after the
// build.
func run(target string, serial string, buildOnly bool) error {
	if target == t4TargetAndroid && serial == "" && !buildOnly {
		fmt.Println("not run: no Android surrogate named (-serial)")
		return nil
	}
	directory, err := os.MkdirTemp("", "t4cell")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	binaryPath := filepath.Join(directory, "sdk.test")
	build, err := cellBuild(target, binaryPath)
	if err != nil {
		return err
	}
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		return err
	}
	if buildOnly {
		fmt.Printf("built the %s cells; not run\n", target)
		return nil
	}
	pattern := "^(" + strings.Join(t4CellTests, "|") + ")$"
	if target == t4TargetHost {
		cells := exec.Command(binaryPath, "-test.v", "-test.count=1", "-test.run", pattern)
		cells.Stdout, cells.Stderr = os.Stdout, os.Stderr
		return cells.Run()
	}
	remotePath := t4AndroidDirectory + "/sdk.test"
	for _, step := range [][]string{
		{"adb", "-s", serial, "push", binaryPath, remotePath},
		{"adb", "-s", serial, "shell", remotePath, "-test.v", "-test.count=1", "-test.run", "'" + pattern + "'"},
		{"adb", "-s", serial, "shell", "rm", "-f", remotePath},
	} {
		command := exec.Command(step[0], step[1:]...)
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
		if err := command.Run(); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(step, " "), err)
		}
	}
	return nil
}
