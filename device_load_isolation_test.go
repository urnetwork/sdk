package sdk

import (
	"os"
	"os/exec"
	"regexp"
	"testing"
)

const isolatedLoadTestEnv = "URNETWORK_SDK_ISOLATED_LOAD_TEST"

// runIsolatedLoadTest reruns a test that measures the process in a fresh copy
// of the current test binary. The SDK's message pools, TLS certificate cache,
// gVisor stacks, and Go heap are process-global; measuring an absolute mobile
// memory ceiling, or reconciling the pools' outstanding roots, after arbitrary
// earlier package tests makes the result depend on test order, and on what
// those tests left winding down, rather than on the workload under test.
//
// It returns true in the parent after the child completed, and false in the
// child so the caller executes its test body exactly once.
func runIsolatedLoadTest(t *testing.T) bool {
	t.Helper()
	if runningIsolated(t) {
		return false
	}

	cmd := exec.Command(
		os.Args[0],
		"-test.run=^"+regexp.QuoteMeta(t.Name())+"$",
		"-test.v",
	)
	cmd.Env = append(os.Environ(), isolatedLoadTestEnv+"="+t.Name())
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated load test failed: %v\n%s", err, output)
	}
	t.Logf("isolated load test:\n%s", output)
	return true
}

// Whether this test runs in the fresh process runIsolatedLoadTest started for
// it, so a fixture that measures the process can refuse to run anywhere else.
func runningIsolated(t *testing.T) bool {
	return os.Getenv(isolatedLoadTestEnv) == t.Name()
}
