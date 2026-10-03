// SPDX-License-Identifier: MPL-2.0
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func toolScript(t *testing.T, executable, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("tool fixtures use POSIX shell scripts")
	}
	must(os.MkdirAll(filepath.Dir(executable), 0755))
	must(os.WriteFile(executable, []byte("#!/bin/sh\n"+body), 0755))
}

func toolQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func fakeDotnet(t *testing.T, version, runtimeVersion, arch string) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dotnet with spaces")
	install := filepath.Join(dir, "libexec")
	executable := filepath.Join(dir, "bin", "dotnet")
	runtimes := "Microsoft.NETCore.App " + runtimeVersion + " [" + filepath.Join(install, "shared", "Microsoft.NETCore.App") + "]"
	toolScript(t, executable, fmt.Sprintf(`case "$1" in
  --version) printf '%%s\n' %s ;;
  --list-runtimes) printf '%%s\n' %s ;;
  --info) printf 'Host:\n  Architecture: %%s\n' %s ;;
  *) exit 94 ;;
esac
`, toolQuote(version), toolQuote(runtimes), toolQuote(arch)))
	return executable, install
}

func TestDotnetRequiresSDKRuntimeAndMatchingArchitecture(t *testing.T) {
	otherArch := "x64"
	if dotnetArch() == "x64" {
		otherArch = "arm64"
	}
	for _, test := range []struct {
		name, version, runtime, arch, failure string
	}{
		{"ready", "8.0.425", "8.0.31", dotnetArch(), ""},
		{"other SDK", "9.0.100", "8.0.31", dotnetArch(), ".NET 8 SDK required"},
		{"preview SDK", "8.0.100-preview.1", "8.0.31", dotnetArch(), ".NET 8 SDK required"},
		{"runtime absent", "8.0.425", "9.0.0", dotnetArch(), "8.0 runtime required"},
		{"preview runtime", "8.0.425", "8.0.0-preview.1", dotnetArch(), "8.0 runtime required"},
		{"wrong architecture", "8.0.425", "8.0.31", otherArch, "host architecture must be"},
		{"unknown architecture", "8.0.425", "8.0.31", "", "host architecture must be"},
	} {
		t.Run(test.name, func(t *testing.T) {
			executable, _ := fakeDotnet(t, test.version, test.runtime, test.arch)
			_, err := inspectDotnet(executable)
			if test.failure == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.failure) {
				t.Fatalf("got %v, want %q", err, test.failure)
			}
		})
	}
}

func TestDotnetLauncherUsesReportedRuntimeRoot(t *testing.T) {
	executable, install := fakeDotnet(t, "8.0.131", "8.0.31", dotnetArch())
	d, err := inspectDotnet(executable)
	if err != nil {
		t.Fatal(err)
	}
	e := d.environment(map[string]string{"NUGET_PACKAGES": "/isolated-cache", "DOTNET_ROOT": "/wrong"})
	if e["DOTNET_ROOT"] != install || e["DOTNET_ROOT_"+strings.ToUpper(dotnetArch())] != install || e["NUGET_PACKAGES"] != "/isolated-cache" {
		t.Fatalf("incorrect consumer environment: %v", e)
	}
}

func TestDotnetFallsBackFromIncompatibleInstallation(t *testing.T) {
	wrong, _ := fakeDotnet(t, "10.0.100", "10.0.0", dotnetArch())
	working, _ := fakeDotnet(t, "8.0.425", "8.0.31", dotnetArch())
	d, err := findDotnet([]string{filepath.Join(t.TempDir(), "missing"), wrong, working})
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(working)
	if err != nil || d.executable != want {
		t.Fatalf("selected %s, want %s: %v", d.executable, want, err)
	}
}

func TestDotnetCheckIsReadOnlyAndExplicitSelectionIsRespected(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-dotnet")
	t.Setenv("SDK_DOTNET", missing)
	if candidates := dotnetCandidates(); !slices.Equal(candidates, []string{missing}) {
		t.Fatalf("explicit SDK_DOTNET was overridden: %v", candidates)
	}
	for _, run := range []func(){func() { checkTools("csharp") }, func() { initTools("csharp") }} {
		failure := panicMessage(run)
		if !strings.Contains(failure, "init") || !strings.Contains(failure, missing) {
			t.Fatalf("missing setup guidance: %s", failure)
		}
		if exists(missing) {
			t.Fatal("invalid explicit tool selection triggered an installation")
		}
	}
}

func TestDotnetInitReusesWorkingSDK(t *testing.T) {
	executable, _ := fakeDotnet(t, "8.0.425", "8.0.31", dotnetArch())
	t.Setenv("SDK_DOTNET", executable)
	// A working installation needs no Homebrew or PATH modification.
	t.Setenv("PATH", t.TempDir())
	initTools("csharp")
	checkTools("csharp")
}

func TestDotnetFindsKegOnlyHomebrewSDK(t *testing.T) {
	bin := t.TempDir()
	prefix := filepath.Join(t.TempDir(), "homebrew with spaces", "opt", "dotnet@8")
	toolScript(t, filepath.Join(bin, "brew"), "[ \"$1\" = --prefix ] && [ \"$2\" = dotnet@8 ] || exit 95\nprintf '%s\\n' "+toolQuote(prefix)+"\n")
	t.Setenv("PATH", bin)
	t.Setenv("SDK_DOTNET", "")
	expected := filepath.Join(prefix, "libexec", "dotnet")
	if candidates := dotnetCandidates(); !slices.Contains(candidates, expected) {
		t.Fatalf("Homebrew SDK missing from candidates: %v", candidates)
	}
}
