package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMobileLifecycleJoinOmissionsAreExplicit(t *testing.T) {
	root := t.TempDir()
	source := strings.Join([]string{
		"// skipped method Api.CloseAndWait with unsupported parameter or return types",
		"// skipped method AsyncLocalState.CloseAndWait with unsupported parameter or return types",
		"// skipped method DeviceLocal.CloseAndWait with unsupported parameter or return types",
	}, "\n")
	if err := os.WriteFile(filepath.Join(root, "Lifecycle.java"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateMobileExports(root); err != nil {
		t.Fatalf("Go-only lifecycle joins were not accepted: %v", err)
	}
}

func TestMobileLifecycleJoinPolicyRejectsAdjacentApiLoss(t *testing.T) {
	root := t.TempDir()
	source := "// skipped method NetworkSpace.CloseAndWait with unsupported parameter or return types\n"
	if err := os.WriteFile(filepath.Join(root, "NetworkSpace.java"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	err := validateMobileExports(root)
	if err == nil {
		t.Fatal("an unreviewed lifecycle omission was accepted")
	}
	if !strings.Contains(err.Error(), "NetworkSpace.CloseAndWait") {
		t.Fatalf("unexpected omission was not identified: %v", err)
	}
}

func TestMobileLifecycleJoinPolicyDoesNotPrefixMatch(t *testing.T) {
	root := t.TempDir()
	source := "// skipped method DeviceLocal.CloseAndWaitForLeak with unsupported parameter or return types\n"
	if err := os.WriteFile(filepath.Join(root, "DeviceLocal.java"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateMobileExports(root); err == nil {
		t.Fatal("a similarly named but unreviewed omission was accepted")
	}
}

func TestMobileExportPolicyRejectsMalformedSkippedRecord(t *testing.T) {
	root := t.TempDir()
	source := "// skipped unexpectedly malformed output\n"
	if err := os.WriteFile(filepath.Join(root, "Broken.java"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateMobileExports(root); err == nil {
		t.Fatal("a malformed skipped record was ignored")
	}
}
