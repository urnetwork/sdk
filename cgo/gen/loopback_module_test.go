package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Release preparation runs tidy and get with the shipping module. Custom build
// tags are enabled by tidy, so the fixture's testdata boundary must exclude its
// server dependencies even on a host with no message-server checkout or network.
// Use the real fixture and overlay, with a dependency-free shipping package, to
// test Go's discovery without changing this checkout's module files.
func TestLoopbackModuleBoundaryAndOverlay(t *testing.T) {
	cgoDirectory := filepath.Dir(testingGenDir(t))
	command := exec.Command("go", "list", "-mod=readonly", "-deps", "-tags=urnet_message_loopback", "./...")
	command.Dir = cgoDirectory
	command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOFLAGS=")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("shipping package discovery with the loopback tag: %v\n%s", err, output)
	}
	if strings.Contains(string(output), "github.com/urnetwork/message-server") {
		t.Fatalf("shipping module includes the loopback server without the overlay:\n%s", output)
	}

	root := filepath.Join(t.TempDir(), "cgo with spaces")
	if err := os.MkdirAll(filepath.Join(root, "ctest", "testdata"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"go.mod":  []byte("module example.invalid/loopback-boundary\n\ngo 1.26.5\n"),
		"main.go": []byte("package main\n\nfunc main() {}\n"),
	}
	for _, name := range []string{
		"ctest/loopback-overlay.json",
		"ctest/testdata/loopback_test_world.go",
	} {
		contents, err := os.ReadFile(filepath.Join(cgoDirectory, name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = contents
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(root, name), contents, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGo := func(t *testing.T, args ...string) []byte {
		t.Helper()
		command := exec.Command("go", args...)
		command.Dir = root
		command.Env = append(os.Environ(),
			"GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOFLAGS=", "CGO_ENABLED=1",
		)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, output)
		}
		return output
	}
	runGo(t, "mod", "tidy")
	runGo(t, "get", "-t", "./...")

	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"tag alone", []string{"-tags=urnet_message_loopback"}, false},
		{"overlay alone", []string{"-overlay=ctest/loopback-overlay.json"}, false},
		{"test build", []string{"-overlay=ctest/loopback-overlay.json", "-tags=urnet_message_loopback"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// -e lets us inspect the test package without resolving its intentionally
			// unavailable dependencies. The real library build checks those separately.
			args := append([]string{"list", "-e", "-json"}, tc.args...)
			output := runGo(t, append(args, ".")...)
			var pkg struct {
				CgoFiles []string
				Imports  []string
			}
			if err := json.Unmarshal(output, &pkg); err != nil {
				t.Fatalf("decode package metadata: %v\n%s", err, output)
			}
			if got := slices.Contains(pkg.CgoFiles, "loopback_test_world.go"); got != tc.want {
				t.Fatalf("loopback fixture present = %v, want %v: %s", got, tc.want, output)
			}
			if got := slices.Contains(pkg.Imports, "github.com/urnetwork/message-server/api"); got != tc.want {
				t.Fatalf("message-server import present = %v, want %v: %s", got, tc.want, output)
			}
		})
	}
}
