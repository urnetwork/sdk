// The T4 cell runner (SDK-M8-15; sidecar §22.4's T4, §22.9's Memory, size row; owner decision 100): builds the sdk's
// cell tests (sidecar_t4_cells_test.go, sidecar_memory_t4_test.go) for a target and runs them there, printing each
// cell's rows: on this host, or on an Android surrogate attached to adb, where the same cells measure the device. A
// target that cannot run here reports not run, which a gate lists as open and never as a pass (§22.4's T1 rule).
//
// The cells boot the app's sidecar role on a network space that discovers nothing and enroll nowhere, so a run reaches
// no host. The iOS surrogate's run needs a signed test host and is external. An Android run takes the device named by
// -serial and no other: a device attached to this machine may be another campaign's (tests/runner/perf's physical
// custody), so the runner never picks one itself.
//
//	go run ./cmd/t4cell -target host
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

// The cells, by test name.
var t4CellTests = []string{
	"TestIos32CellBootsWithinItsBudget",
	"TestAndroidCellRunsNoBackgroundSidecar",
	"TestT4BudgetCatchesADeliberateOverAllocation",
	"TestForwardingCompileAdmissionOnTheIos32Cell",
}

// The targets, as -target names them.
const (
	t4TargetHost    = "host"
	t4TargetAndroid = "android"
)

// Where an Android surrogate's run keeps its binary.
const t4AndroidDirectory = "/data/local/tmp"

func main() {
	target := flag.String("target", t4TargetHost, "host, or android for a surrogate attached to adb")
	serial := flag.String("serial", "", "android: the surrogate's adb serial; required, since no device is picked")
	flag.Parse()
	if err := run(*target, *serial); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Builds the cells for target and runs them there: on android, on the device serial names.
func run(target string, serial string) error {
	directory, err := os.MkdirTemp("", "t4cell")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	binaryPath := filepath.Join(directory, "sdk.test")
	build := exec.Command("go", "test", "-c", "-o", binaryPath, "github.com/urnetwork/sdk")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	pattern := "^(" + strings.Join(t4CellTests, "|") + ")$"
	switch target {
	case t4TargetHost:
		if err := build.Run(); err != nil {
			return err
		}
		cells := exec.Command(binaryPath, "-test.v", "-test.count=1", "-test.run", pattern)
		cells.Stdout, cells.Stderr = os.Stdout, os.Stderr
		return cells.Run()
	case t4TargetAndroid:
		if serial == "" {
			fmt.Println("not run: no Android surrogate named (-serial)")
			return nil
		}
		build.Env = append(os.Environ(), "GOOS=android", "GOARCH=arm64", "CGO_ENABLED=0")
		if err := build.Run(); err != nil {
			return err
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
	return errors.New("a target is host or android")
}
