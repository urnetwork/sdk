package buildcontract

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const fakeGo = `#!/bin/sh
set -eu
printf 'go %s/%s\n' "${GOOS:-host}" "${GOARCH:-host}" >>"$BUILD_CONTRACT_LOG"
output=''
want_output=0
for arg in "$@"; do
  if [ "$want_output" = 1 ]; then
    output="$arg"
    want_output=0
  elif [ "$arg" = '-o' ]; then
    want_output=1
  fi
done
if [ "${FAIL_ALL_GO:-0}" = 1 ] || {
  [ "${FAIL_GOOS:-}" = "${GOOS:-}" ] && [ "${FAIL_GOARCH:-}" = "${GOARCH:-}" ]
}; then
  exit 47
fi
if [ -n "$output" ]; then
  mkdir -p "$(dirname "$output")"
  printf 'new artifact\n' >"$output"
fi
`

const fakeZip = `#!/bin/sh
set -eu
printf 'zip\n' >>"$BUILD_CONTRACT_LOG"
if [ "${FAIL_ZIP:-0}" = 1 ]; then
  exit 53
fi
output=''
for arg in "$@"; do
  case "$arg" in
    -*) ;;
    *) output="$arg"; break ;;
  esac
done
[ -n "$output" ]
printf 'archive\n' >"$output"
`

func cgoMakefile(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate build contract test")
	}
	path := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "Makefile"))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("locate cgo Makefile: %v", err)
	}
	return path
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

func newMakeFixture(t *testing.T) (dir, logPath string) {
	t.Helper()
	dir = t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(filepath.Join(dir, "include"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"urnetwork_sdk.h", "urnetwork_sdk.hpp", "urnetwork_sdk.def"} {
		if err := os.WriteFile(filepath.Join(dir, "include", name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(binDir, "go"), fakeGo)
	writeExecutable(t, filepath.Join(binDir, "zip"), fakeZip)
	logPath = filepath.Join(dir, "commands.log")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BUILD_CONTRACT_LOG", logPath)
	return dir, logPath
}

func runMake(t *testing.T, dir, target string, extraEnv ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("make", "-f", cgoMakefile(t), target)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	return cmd.CombinedOutput()
}

func requireExitCode(t *testing.T, err error, code int, output []byte) {
	t.Helper()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != code {
		t.Fatalf("exit = %v, want %d; output:\n%s", err, code, output)
	}
}

func readCommandLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Every architecture recipe must stop on its own failed go build. Removing the
// destination before compilation also makes it impossible for a previous .so
// or DLL to survive and be archived as the current build.
func TestArchitectureBuildFailureStopsBeforeArchiveAndRejectsStaleOutput(t *testing.T) {
	tests := []struct {
		name      string
		target    string
		goos      string
		failArch  string
		stalePath string
		wantLog   string
	}{
		{
			name:      "linux amd64",
			target:    "build_linux",
			goos:      "linux",
			failArch:  "amd64",
			stalePath: "build/linux/amd64/libURnetworkSdk.so",
			wantLog:   "go linux/amd64\n",
		},
		{
			name:      "linux arm64",
			target:    "build_linux",
			goos:      "linux",
			failArch:  "arm64",
			stalePath: "build/linux/arm64/libURnetworkSdk.so",
			wantLog:   "go linux/amd64\ngo linux/arm64\n",
		},
		{
			name:      "windows amd64",
			target:    "build_windows",
			goos:      "windows",
			failArch:  "amd64",
			stalePath: "build/windows/amd64/URnetworkSdk.dll",
			wantLog:   "go windows/amd64\n",
		},
		{
			name:      "windows arm64",
			target:    "build_windows",
			goos:      "windows",
			failArch:  "arm64",
			stalePath: "build/windows/arm64/URnetworkSdk.dll",
			wantLog:   "go windows/amd64\ngo windows/arm64\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir, logPath := newMakeFixture(t)
			stalePath := filepath.Join(dir, test.stalePath)
			if err := os.MkdirAll(filepath.Dir(stalePath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(stalePath, []byte("stale artifact\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			output, err := runMake(
				t,
				dir,
				test.target,
				"FAIL_GOOS="+test.goos,
				"FAIL_GOARCH="+test.failArch,
			)
			requireExitCode(t, err, 2, output)
			if got := readCommandLog(t, logPath); got != test.wantLog {
				t.Fatalf("commands = %q, want %q", got, test.wantLog)
			}
			if _, err := os.Stat(stalePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed build retained stale output: %v", err)
			}
			if strings.Contains(readCommandLog(t, logPath), "zip") {
				t.Fatal("archive ran after a failed architecture build")
			}
		})
	}
}

func TestArchiveFailurePropagatesForEveryDesktopTarget(t *testing.T) {
	for _, target := range []string{"build_linux", "build_windows"} {
		t.Run(target, func(t *testing.T) {
			dir, logPath := newMakeFixture(t)
			output, err := runMake(t, dir, target, "FAIL_ZIP=1")
			requireExitCode(t, err, 2, output)
			if !strings.HasSuffix(readCommandLog(t, logPath), "zip\n") {
				t.Fatal("archive command did not run")
			}
		})
	}
}

func TestHostBuildFailureRejectsStaleLibrary(t *testing.T) {
	dir, logPath := newMakeFixture(t)
	stalePath := filepath.Join(dir, "build", "host", "libURnetworkSdk.dylib")
	if err := os.MkdirAll(filepath.Dir(stalePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stalePath, []byte("stale artifact\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := runMake(t, dir, "build_host", "FAIL_ALL_GO=1")
	requireExitCode(t, err, 2, output)
	if got := readCommandLog(t, logPath); got != "go host/host\n" {
		t.Fatalf("commands = %q, want host go build", got)
	}
	if _, err := os.Stat(stalePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed host build retained stale output: %v", err)
	}
}
