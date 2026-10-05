// Checks the environment the mobile bind recipes give the Go tools they run.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A GODEBUG that a recipe exports reaches every Go program the recipe runs:
// go, gomobile, gobind, checksec and the mobileexports check. Go removes
// settings over time, and from go 1.27 a program exits at startup when the
// environment sets a removed setting to its old value. The gotypesalias=0
// these recipes exported is one, so under go 1.27 both builds stopped at their
// first Go program. make -n prints both recipes without running them; a stub
// go answers the go list that make runs while it reads the Makefile.
func TestMobileBindRecipesExportNoGodebug(t *testing.T) {
	makefile, err := filepath.Abs("Makefile")
	testingBuildNoError(t, err)
	stubDirectory := t.TempDir()
	testingBuildNoError(t, os.WriteFile(filepath.Join(stubDirectory, "go"), []byte("#!/bin/sh\n"), 0o755))
	command := exec.CommandContext(t.Context(), "make", "-n", "-f", makefile, "_build_android", "build_apple")
	command.Dir = t.TempDir()
	command.Env = append(
		testingBuildEnvironmentWithoutAndroidLock(),
		"PATH="+stubDirectory+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "gomobile bind") {
		t.Fatalf("make -n printed no gomobile bind:\n%s", output)
	}
	godebugAssignment := regexp.MustCompile(`(^|[^A-Za-z0-9_])GODEBUG=`)
	for _, line := range strings.Split(string(output), "\n") {
		// recipe comments reach the shell as comment lines
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if godebugAssignment.MatchString(line) {
			t.Errorf("a mobile bind recipe sets GODEBUG for the tools it runs: %s", strings.TrimSpace(line))
		}
	}
}
