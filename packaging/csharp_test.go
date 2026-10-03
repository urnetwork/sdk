// SPDX-License-Identifier: MPL-2.0
package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCSharpPackagingDoesNotStartPersistentBuildServers(t *testing.T) {
	sourceRoot := root
	root = t.TempDir()
	t.Cleanup(func() { root = sourceRoot })
	t.Setenv("SDK_PACKAGE_VERSION", "1.2.3")
	copyFile(filepath.Join(sourceRoot, "csharp/global.json"), path("csharp/global.json"))
	textFile(path("csharp/URnetwork.SDK.csproj"), "<Project />\n")
	textFile(path("LICENSE"), "fixture license\n")

	bin := t.TempDir()
	executable := filepath.Join(bin, "dotnet")
	log := filepath.Join(bin, "commands")
	// The fixture models the persistent-server choice without creating a real
	// orphan. Exercise the actual pack/check paths, not a duplicate argument list.
	toolScript(t, executable, fmt.Sprintf(`
case "$1" in
  --version) printf '8.0.131\n'; exit 0 ;;
  --list-runtimes) printf 'Microsoft.NETCore.App 8.0.31 [%%s/shared/Microsoft.NETCore.App]\n' %s; exit 0 ;;
  --info) printf 'Host:\n  Architecture: %%s\n' %s; exit 0 ;;
esac
action="$1"
shift
case "$action" in
  pack|run)
    disabled=no
    for argument in "$@"; do
      if [ "$argument" = --disable-build-servers ]; then disabled=yes; fi
    done
    if [ "$disabled" != yes ]; then
      printf 'persistent build server would outlive %%s\n' "$action" >&2
      exit 97
    fi
    ;;
  new|add) ;;
  *) exit 94 ;;
esac
printf '%%s\n' "$action" >> %s
if [ "$action" = pack ]; then
  while [ "$#" -gt 0 ]; do
    if [ "$1" = -o ]; then
      shift
      printf 'fixture NuGet artifact\n' > "$1/URnetwork.SDK.1.2.3.nupkg"
      exit 0
    fi
    shift
  done
  exit 95
fi
`, toolQuote(bin), toolQuote(dotnetArch()), toolQuote(log)))
	// A missing guard must fail deterministically for either compiler entry
	// point, even on machines where an existing Roslyn server masks the leak.
	for _, action := range []string{"pack", "run"} {
		cmd := exec.Command(executable, action)
		if output, err := cmd.CombinedOutput(); err == nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 97 {
			t.Fatalf("unguarded %s was not rejected: %v, %s", action, err, output)
		}
	}
	t.Setenv("SDK_DOTNET", executable)
	lib := path("fixture-native.so")
	textFile(lib, "fixture native library\n")
	m := nativeManifest{ABI: 1, Version: "1.2.3", Libraries: []library{{
		Platform: "linux-amd64", Path: lib, SHA256: hash(lib), MinimumOS: "glibc-2.31",
	}}}
	out := path("csharp/dist")
	buildPackage("csharp", m, out)
	checkPackage("csharp", out)
	if got := string(read(log)); got != "pack\nnew\nadd\nrun\n" {
		t.Fatalf("unexpected C# command sequence: %q", got)
	}
	verifiedFiles(out, "1.2.3", true)
}
